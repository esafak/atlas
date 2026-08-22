// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package mysql

import (
	"context"
	"encoding/binary"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"ariga.io/atlas/sql/internal/sqlx"
	"ariga.io/atlas/sql/migrate"
	"ariga.io/atlas/sql/schema"
)

type (
	// tplanApply decorates MySQL planApply.
	tplanApply struct{ planApply }
	// tdiff decorates MySQL diff.
	tdiff struct{ diff }
	// tinspect decorates MySQL inspect.
	tinspect struct{ inspect }

	// AutoRandom describes the TiDB AUTO_RANDOM column attribute.
	// Bits is the shard-bit count. TiDB normalizes bare AUTO_RANDOM to 5.
	AutoRandom struct {
		schema.Attr
		Bits int
	}
)

const defaultAutoRandomBits = 5

func (d *tdiff) ColumnChange(fromT *schema.Table, from, to *schema.Column, opts *schema.DiffOptions) (schema.Change, error) {
	change, err := d.diff.ColumnChange(fromT, from, to, opts)
	if err != nil {
		return nil, err
	}
	if !autoRandomChanged(from, to) && !hasAutoRandom(from) && !hasAutoRandom(to) {
		return change, nil
	}
	if change != sqlx.NoChange || autoRandomChanged(from, to) {
		return nil, fmt.Errorf("TiDB does not support altering AUTO_RANDOM on column %q; recreate the column or table", to.Name)
	}
	return change, nil
}

func hasAutoRandom(c *schema.Column) bool {
	_, ok := autoRandomAttr(c.Attrs)
	return ok
}

func autoRandomChanged(from, to *schema.Column) bool {
	fromAttr, fromOK := autoRandomAttr(from.Attrs)
	toAttr, toOK := autoRandomAttr(to.Attrs)
	return fromOK != toOK || fromOK && !autoRandomBitsEqual(fromAttr.Bits, toAttr.Bits)
}

func autoRandomAttr(attrs []schema.Attr) (AutoRandom, bool) {
	a := AutoRandom{}
	return a, sqlx.Has(attrs, &a)
}

func autoRandomBitsEqual(from, to int) bool {
	// TiDB normalizes bare AUTO_RANDOM to AUTO_RANDOM(5) in SHOW CREATE.
	return normalizeAutoRandomBits(from) == normalizeAutoRandomBits(to)
}

func normalizeAutoRandomBits(bits int) int {
	if bits == 0 {
		return defaultAutoRandomBits
	}
	return bits
}

// priority computes the priority of each change.
//
// TiDB does not support multischema ALTERs (i.e. multiple changes in a single ALTER statement).
// Therefore, we have to break down each alter. This function helps order the ALTERs so they work.
// e.g. priority gives precedence to DropForeignKey over DropColumn, because a column cannot be
// dropped if its foreign key was not dropped before.
func priority(change schema.Change) int {
	switch c := change.(type) {
	case *schema.ModifyTable:
		// each modifyTable should have a single change since we apply `flat` before we sort.
		return priority(c.Changes[0])
	case *schema.ModifySchema:
		// each modifyTable should have a single change since we apply `flat` before we sort.
		return priority(c.Changes[0])
	case *schema.AddColumn:
		return 1
	case *schema.DropIndex, *schema.DropForeignKey, *schema.DropAttr, *schema.DropCheck:
		return 2
	case *schema.ModifyIndex, *schema.ModifyForeignKey:
		return 3
	default:
		return 4
	}
}

// flat takes a list of changes and breaks them down to single atomic changes (e.g: no ModifyTable
// with multiple AddColumn inside it). Note that, the only "changes" that include sub-changes are
// `ModifyTable` and `ModifySchema`.
func flat(changes []schema.Change) []schema.Change {
	var flat []schema.Change
	for _, change := range changes {
		switch m := change.(type) {
		case *schema.ModifyTable:
			for _, c := range m.Changes {
				flat = append(flat, &schema.ModifyTable{
					T:       m.T,
					Changes: []schema.Change{c},
				})
			}
		case *schema.ModifySchema:
			for _, c := range m.Changes {
				flat = append(flat, &schema.ModifySchema{
					S:       m.S,
					Changes: []schema.Change{c},
				})
			}
		default:
			flat = append(flat, change)
		}
	}
	return flat
}

// PlanChanges returns a migration plan for the given schema changes.
func (p *tplanApply) PlanChanges(ctx context.Context, name string, changes []schema.Change, opts ...migrate.PlanOption) (*migrate.Plan, error) {
	planned, err := sqlx.DetachCycles(changes)
	if err != nil {
		return nil, err
	}
	planned = flat(planned)
	sort.SliceStable(planned, func(i, j int) bool {
		return priority(planned[i]) < priority(planned[j])
	})
	s := &state{
		conn: p.conn,
		Plan: migrate.Plan{
			Name: name,
			// A plan is reversible, if all
			// its changes are reversible.
			Reversible:    true,
			Transactional: false,
		},
	}
	for _, c := range planned {
		// Use the planner of MySQL with each "atomic" change.
		plan, err := p.planApply.PlanChanges(ctx, name, []schema.Change{c}, opts...)
		if err != nil {
			return nil, err
		}
		if !plan.Reversible {
			s.Plan.Reversible = false
		}
		s.Plan.Changes = append(s.Plan.Changes, plan.Changes...)
	}
	return &s.Plan, nil
}

func (p *tplanApply) ApplyChanges(ctx context.Context, changes []schema.Change, opts ...migrate.PlanOption) error {
	return sqlx.ApplyChanges(ctx, changes, p, opts...)
}

func (i *tinspect) InspectSchema(ctx context.Context, name string, opts *schema.InspectOptions) (*schema.Schema, error) {
	s, err := i.inspect.InspectSchema(ctx, name, opts)
	if err != nil {
		return nil, err
	}
	return i.patchSchema(ctx, s)
}

func (i *tinspect) InspectRealm(ctx context.Context, opts *schema.InspectRealmOption) (*schema.Realm, error) {
	r, err := i.inspect.InspectRealm(ctx, opts)
	if err != nil {
		return nil, err
	}
	for _, s := range r.Schemas {
		if _, err := i.patchSchema(ctx, s); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (i *tinspect) patchSchema(ctx context.Context, s *schema.Schema) (*schema.Schema, error) {
	for _, t := range s.Tables {
		filterAutoRandomBase(t)
		var createStmt CreateStmt
		if ok := sqlx.Has(t.Attrs, &createStmt); !ok {
			if _, err := i.createStmt(ctx, t); err != nil {
				return nil, err
			}
		}
		if err := i.setCollate(t); err != nil {
			return nil, err
		}
		if err := i.setAutoIncrement(t); err != nil {
			return nil, err
		}
		for _, c := range t.Columns {
			if err := i.patchColumn(ctx, t, c); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}

var reAutoRandom = regexp.MustCompile(`(?i)/\*\s*T!\s*\[\s*auto_rand\s*\]\s+AUTO_RANDOM(?:\s*\(\s*(\d+)(?:\s*,\s*(\d+))?\s*\))?\s*\*/`)
var reAutoRandomBase = regexp.MustCompile(`(?i)(?:^|\s)AUTO_RANDOM_BASE\s*=\s*\d+`)

func filterAutoRandomBase(t *schema.Table) {
	for i := 0; i < len(t.Attrs); i++ {
		a, ok := t.Attrs[i].(*CreateOptions)
		if !ok {
			continue
		}
		a.V = strings.TrimSpace(reAutoRandomBase.ReplaceAllString(a.V, ""))
		if a.V == "" {
			t.Attrs = append(t.Attrs[:i], t.Attrs[i+1:]...)
			i--
		}
	}
}

func (i *tinspect) patchColumn(_ context.Context, t *schema.Table, c *schema.Column) error {
	var stmt CreateStmt
	if sqlx.Has(t.Attrs, &stmt) {
		for _, line := range strings.Split(stmt.S, "\n") {
			if !strings.Contains(line, "`"+c.Name+"`") {
				continue
			}
			if matches := reAutoRandom.FindStringSubmatch(line); len(matches) > 0 {
				if matches[2] != "" {
					return fmt.Errorf("unsupported TiDB AUTO_RANDOM(S, R) on column %q: only AUTO_RANDOM(S) is supported", c.Name)
				}
				bits := defaultAutoRandomBits
				a := &AutoRandom{}
				if matches[1] != "" {
					var err error
					bits, err = strconv.Atoi(matches[1])
					if err != nil {
						return fmt.Errorf("invalid TiDB AUTO_RANDOM bit count %q on column %q: %w", matches[1], c.Name, err)
					}
				}
				if bits < 1 || bits > 15 {
					return fmt.Errorf("invalid TiDB AUTO_RANDOM bit count %d on column %q: want 1..15", bits, c.Name)
				}
				a.Bits = bits
				schema.ReplaceOrAppend(&c.Attrs, a)
				break
			}
		}
	}
	_, ok := c.Type.Type.(*BitType)
	if !ok {
		return nil
	}
	// TiDB has a bug where it does not format bit default value correctly.
	if lit, ok := c.Default.(*schema.Literal); ok && !strings.HasPrefix(lit.V, "b'") {
		lit.V = bytesToBitLiteral([]byte(lit.V))
	}
	return nil
}

// bytesToBitLiteral converts a bytes to MySQL bit literal.
// e.g. []byte{4} -> b'100', []byte{2,1} -> b'1000000001'.
// See: https://github.com/pingcap/tidb/issues/32655.
func bytesToBitLiteral(b []byte) string {
	bytes := make([]byte, 8)
	for i := 0; i < len(b); i++ {
		bytes[8-len(b)+i] = b[i]
	}
	val := binary.BigEndian.Uint64(bytes)
	return fmt.Sprintf("b'%b'", val)
}

// e.g CHARSET=utf8mb4 COLLATE=utf8mb4_bin
var reColl = regexp.MustCompile(`(?i)CHARSET\s*=\s*(\w+)\s*COLLATE\s*=\s*(\w+)`)

// setCollate extracts the updated Collation from CREATE TABLE statement.
func (i *tinspect) setCollate(t *schema.Table) error {
	var c CreateStmt
	if !sqlx.Has(t.Attrs, &c) {
		return fmt.Errorf("missing CREATE TABLE statement in attributes for %q", t.Name)
	}
	matches := reColl.FindStringSubmatch(c.S)
	if len(matches) != 3 {
		return fmt.Errorf("missing COLLATE and/or CHARSET information on CREATE TABLE statement for %q", t.Name)
	}
	t.SetCharset(matches[1])
	t.SetCollation(matches[2])
	return nil
}

// setCollate extracts the updated Collation from CREATE TABLE statement.
func (i *tinspect) setAutoIncrement(t *schema.Table) error {
	// patch only it is set (set falsely to '1' due to this bug:https://github.com/pingcap/tidb/issues/24702).
	ai := &AutoIncrement{}
	if !sqlx.Has(t.Attrs, ai) {
		return nil
	}
	var c CreateStmt
	if !sqlx.Has(t.Attrs, &c) {
		return fmt.Errorf("missing CREATE TABLE statement in attributes for %q", t.Name)
	}
	matches := reAutoinc.FindStringSubmatch(c.S)
	if len(matches) != 2 {
		return nil
	}
	v, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		return err
	}
	ai.V = v
	schema.ReplaceOrAppend(&t.Attrs, ai)
	return nil
}
