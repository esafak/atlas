// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package mysql

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"ariga.io/atlas/sql/internal/sqlx"
	"ariga.io/atlas/sql/mysql/internal/mysqlversion"
	"ariga.io/atlas/sql/schema"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestTiDBPatchAutoRandom(t *testing.T) {
	table := &schema.Table{
		Attrs: []schema.Attr{&CreateStmt{S: "CREATE TABLE `users` (\n  `bare` bigint NOT NULL /*T![auto_rand] AUTO_RANDOM */ ,\n  `bits` bigint NOT NULL /*T![auto_rand] AUTO_RANDOM(5) */\n)"}},
	}
	bare := &schema.Column{Name: "bare", Type: &schema.ColumnType{Type: &schema.IntegerType{T: "bigint"}}}
	bits := &schema.Column{Name: "bits", Type: &schema.ColumnType{Type: &schema.IntegerType{T: "bigint"}}}
	i := &tinspect{}
	require.NoError(t, i.patchColumn(context.Background(), table, bare))
	require.NoError(t, i.patchColumn(context.Background(), table, bits))

	a := &AutoRandom{}
	require.True(t, sqlx.Has(bare.Attrs, a))
	require.Equal(t, defaultAutoRandomBits, a.Bits)
	require.True(t, sqlx.Has(bits.Attrs, a))
	require.Equal(t, 5, a.Bits)
}

var fresnelAutoRandomCorpusFiles = []string{
	"global/01_organizations.sql",
	"global/01_roles.sql",
	"global/02_product_users.sql",
	"global/03_announcements.sql",
	"global/03_assets.sql",
	"global/03_communities.sql",
	"global/03_linked_users.sql",
	"global/03_passkey_credentials.sql",
	"global/03_user_roles.sql",
	"global/04_asset_objects.sql",
	"global/04_invitations.sql",
	"tenant/01_users.sql",
	"tenant/02_announcements.sql",
	"tenant/02_assets.sql",
	"tenant/02_discussions.sql",
	"tenant/02_subscriptions.sql",
	"tenant/03_asset_objects.sql",
	"tenant/03_credentials.sql",
	"tenant/03_discussion_summaries.sql",
	"tenant/03_discussion_summary_invalidations.sql",
	"tenant/03_groups.sql",
	"tenant/03_rooms.sql",
	"tenant/03_tags.sql",
	"tenant/04_discussion_tags.sql",
	"tenant/04_group_users.sql",
	"tenant/04_posts.sql",
	"tenant/05_post_feedback.sql",
	"tenant/06_authz_principals.sql",
	"tenant/07_authz_resources.sql",
	"tenant/08_authz_principal_memberships.sql",
	"tenant/09_authz_resource_grants.sql",
	"tenant/11_authz_external_bindings.sql",
	"tenant/12_authz_outbox.sql",
}

func TestFresnelAutoRandomCorpusFixture(t *testing.T) {
	const source = "BIGINT /* [jooq ignore start] */ auto_random /* [jooq ignore stop] */"
	require.Len(t, fresnelAutoRandomCorpusFiles, 33)
	for _, path := range fresnelAutoRandomCorpusFiles {
		t.Run(path, func(t *testing.T) {
			table := &schema.Table{Attrs: []schema.Attr{&CreateStmt{
				S: "CREATE TABLE `users` (`id` " + source + " NOT NULL /*T![auto_rand] AUTO_RANDOM(5) */ PRIMARY KEY)",
			}}}
			column := &schema.Column{Name: "id", Type: &schema.ColumnType{Type: &schema.IntegerType{T: "bigint"}}}
			require.NoError(t, (&tinspect{}).patchColumn(context.Background(), table, column))
			a := &AutoRandom{}
			require.True(t, sqlx.Has(column.Attrs, a))
			require.Equal(t, defaultAutoRandomBits, a.Bits)
		})
	}
}

func TestTiDBFilterAutoRandomBase(t *testing.T) {
	table := &schema.Table{Attrs: []schema.Attr{
		&CreateOptions{V: `AUTO_RANDOM_BASE=123 COMPRESSION="ZLIB"`},
	}}
	filterAutoRandomBase(table)
	options := &CreateOptions{}
	require.True(t, sqlx.Has(table.Attrs, options))
	require.Equal(t, `COMPRESSION="ZLIB"`, options.V)

	table.Attrs = []schema.Attr{&CreateOptions{V: "AUTO_RANDOM_BASE=123"}}
	filterAutoRandomBase(table)
	require.Empty(t, table.Attrs)
}

func TestTiDBAutoRandomHCL(t *testing.T) {
	s := schema.New("test").AddTables(
		schema.NewTable("users").AddColumns(
			&schema.Column{
				Name:  "bare",
				Type:  &schema.ColumnType{Type: &schema.IntegerType{T: "bigint"}},
				Attrs: []schema.Attr{&AutoRandom{}},
			},
			&schema.Column{
				Name:  "bits",
				Type:  &schema.ColumnType{Type: &schema.IntegerType{T: "bigint"}},
				Attrs: []schema.Attr{&AutoRandom{Bits: 5}},
			},
		),
	)
	got, err := MarshalHCL(s)
	require.NoError(t, err)
	require.NotContains(t, string(got), "auto_random = true")
	require.Contains(t, string(got), "auto_random = 5")
	var bare schema.Schema
	bareHCL := []byte(strings.Replace(string(got), "auto_random = 5", "auto_random = true", 1))
	require.NoError(t, EvalHCLBytes(bareHCL, &bare, nil))
	a := &AutoRandom{}
	require.True(t, sqlx.Has(bare.Tables[0].Columns[0].Attrs, a))
	require.Equal(t, defaultAutoRandomBits, a.Bits)
	for _, value := range []string{"false", "0", "16"} {
		var invalid schema.Schema
		badHCL := []byte(strings.Replace(string(got), "auto_random = 5", "auto_random = "+value, 1))
		require.Error(t, EvalHCLBytes(badHCL, &invalid, nil), value)
	}

	var roundTrip schema.Schema
	require.NoError(t, EvalHCLBytes(got, &roundTrip, nil))
	require.Len(t, roundTrip.Tables, 1)
	for i, bits := range []int{defaultAutoRandomBits, 5} {
		a := &AutoRandom{}
		require.True(t, sqlx.Has(roundTrip.Tables[0].Columns[i].Attrs, a))
		require.Equal(t, bits, a.Bits)
	}
}

func TestTiDBAutoRandomPlan(t *testing.T) {
	for _, tt := range []struct {
		name string
		attr *AutoRandom
		want string
	}{
		{name: "bare", attr: &AutoRandom{}, want: "/*T![auto_rand] AUTO_RANDOM(5) */"},
		{name: "bits", attr: &AutoRandom{Bits: 5}, want: "/*T![auto_rand] AUTO_RANDOM(5) */"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := &tplanApply{planApply{conn: &conn{V: "5.7.25-TiDB-v6.6.0"}}}
			table := schema.NewTable("users").AddColumns(
				&schema.Column{
					Name:  "id",
					Type:  &schema.ColumnType{Type: &schema.IntegerType{T: "bigint"}},
					Attrs: []schema.Attr{tt.attr},
				},
			)
			plan, err := d.PlanChanges(context.Background(), "test", []schema.Change{&schema.AddTable{T: table}})
			require.NoError(t, err)
			require.Equal(t, "CREATE TABLE `users` (`id` bigint NOT NULL "+tt.want+")", plan.Changes[0].Cmd)
		})
	}
}

func TestTiDBAutoRandomConversion(t *testing.T) {
	from := &schema.Column{
		Name:  "id",
		Type:  &schema.ColumnType{Type: &schema.IntegerType{T: TypeBigInt}},
		Attrs: []schema.Attr{&AutoIncrement{}},
	}
	to := &schema.Column{
		Name:  from.Name,
		Type:  &schema.ColumnType{Type: &schema.IntegerType{T: TypeBigInt}},
		Attrs: []schema.Attr{&AutoRandom{Bits: 5}},
	}
	table := schema.NewTable("users").AddColumns(from)
	table.SetPrimaryKey(schema.NewPrimaryKey(from))
	table.AddAttrs(&CreateStmt{S: "CREATE TABLE `users` (\n  `id` bigint NOT NULL AUTO_INCREMENT,\n  PRIMARY KEY (`id`) /*T![clustered_index] CLUSTERED */\n)"})

	d := &tdiff{diff{conn: &conn{V: "5.7.25-TiDB-v6.6.0"}}}
	change, err := d.ColumnChange(table, from, to, nil)
	require.NoError(t, err)
	require.IsType(t, &schema.ModifyColumn{}, change)

	planner := &tplanApply{planApply{conn: &conn{V: "5.7.25-TiDB-v6.6.0"}}}
	plan, err := planner.PlanChanges(context.Background(), "test", []schema.Change{
		&schema.ModifyTable{T: table, Changes: []schema.Change{change}},
	})
	require.NoError(t, err)
	require.Equal(t, "ALTER TABLE `users` MODIFY COLUMN `id` bigint NOT NULL /*T![auto_rand] AUTO_RANDOM(5) */", plan.Changes[0].Cmd)
	require.False(t, plan.Reversible)
	require.Nil(t, plan.Changes[0].Reverse)
}

func TestTiDBAutoRandomConversionEnablesSessionSetting(t *testing.T) {
	from := &schema.Column{
		Name:  "id",
		Type:  &schema.ColumnType{Type: &schema.IntegerType{T: TypeBigInt}},
		Attrs: []schema.Attr{&AutoIncrement{}},
	}
	to := &schema.Column{
		Name:  from.Name,
		Type:  &schema.ColumnType{Type: &schema.IntegerType{T: TypeBigInt}},
		Attrs: []schema.Attr{&AutoRandom{Bits: 5}},
	}
	table := schema.NewTable("users").AddColumns(from)
	table.SetPrimaryKey(schema.NewPrimaryKey(from))
	table.AddAttrs(&CreateStmt{S: "CREATE TABLE `users` (\n  `id` bigint NOT NULL AUTO_INCREMENT,\n  PRIMARY KEY (`id`) /*T![clustered_index] CLUSTERED */\n)"})
	desired := schema.NewTable("users").AddColumns(to)

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectExec("SET SESSION tidb_allow_remove_auto_inc = ON").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("ALTER TABLE `users` MODIFY COLUMN `id` bigint NOT NULL /*T![auto_rand] AUTO_RANDOM(5) */")).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("SET SESSION tidb_allow_remove_auto_inc = OFF").WillReturnResult(sqlmock.NewResult(0, 0))

	planner := &tplanApply{planApply{conn: &conn{
		ExecQuerier: db,
		V:           "5.7.25-TiDB-v6.6.0",
	}}}
	err = planner.ApplyChanges(context.Background(), []schema.Change{
		&schema.ModifyTable{T: desired, Changes: []schema.Change{
			&schema.ModifyColumn{Change: schema.ChangeAttr, From: from, To: to},
		}},
	})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestTiDBVersionedAutoRandomAlterEnablesSessionSetting(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectExec("SET SESSION tidb_allow_remove_auto_inc = ON").WillReturnResult(sqlmock.NewResult(0, 0))
	query := "ALTER TABLE `users` MODIFY COLUMN `id` bigint NOT NULL /*T![auto_rand] AUTO_RANDOM(5) */"
	mock.ExpectExec(regexp.QuoteMeta(query)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("SET SESSION tidb_allow_remove_auto_inc = OFF").WillReturnResult(sqlmock.NewResult(0, 0))

	d := &Driver{conn: &conn{
		ExecQuerier: db,
		V:           "5.7.25-TiDB-v6.6.0",
	}}
	_, err = d.ExecContext(context.Background(), query)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestTiDBVersionedUnrelatedAlterSkipsSessionSetting(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	query := "ALTER TABLE `users` RENAME COLUMN `auto_random_flag` TO `flag`"
	mock.ExpectExec(regexp.QuoteMeta(query)).WillReturnResult(sqlmock.NewResult(0, 0))

	d := &Driver{conn: &conn{
		ExecQuerier: db,
		V:           "5.7.25-TiDB-v6.6.0",
	}}
	_, err = d.ExecContext(context.Background(), query)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestTiDBAutoRandomConversionRequiresClusteredAutoIncrementPrimaryKey(t *testing.T) {
	for _, tt := range []struct {
		name              string
		attrs             []schema.Attr
		fromAutoIncrement bool
	}{
		{name: "non-clustered", fromAutoIncrement: true, attrs: []schema.Attr{&CreateStmt{S: "CREATE TABLE `users` (`id` bigint NOT NULL AUTO_INCREMENT, PRIMARY KEY (`id`) /*T![clustered_index] NONCLUSTERED */)"}}},
		{name: "not-auto-increment", attrs: []schema.Attr{&CreateStmt{S: "CREATE TABLE `users` (`id` bigint NOT NULL, PRIMARY KEY (`id`) /*T![clustered_index] CLUSTERED */)"}}},
		{name: "not-primary-key", fromAutoIncrement: true, attrs: []schema.Attr{&CreateStmt{S: "CREATE TABLE `users` (`id` bigint NOT NULL AUTO_INCREMENT)"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			from := &schema.Column{
				Name: "id",
				Type: &schema.ColumnType{Type: &schema.IntegerType{T: TypeBigInt}},
			}
			if tt.fromAutoIncrement {
				from.Attrs = []schema.Attr{&AutoIncrement{}}
			}
			to := &schema.Column{
				Name:  from.Name,
				Type:  &schema.ColumnType{Type: &schema.IntegerType{T: TypeBigInt}},
				Attrs: []schema.Attr{&AutoRandom{Bits: 5}},
			}
			table := schema.NewTable("users").AddColumns(from).AddAttrs(tt.attrs...)
			if tt.name != "not-primary-key" {
				table.SetPrimaryKey(schema.NewPrimaryKey(from))
			}

			d := &tdiff{diff{conn: &conn{V: "5.7.25-TiDB-v6.6.0"}}}
			_, err := d.ColumnChange(table, from, to, nil)
			require.EqualError(t, err, `TiDB does not support altering AUTO_RANDOM on column "id"; recreate the column or table`)
		})
	}
}

func TestTiDBAutoRandomShardBitsIncrease(t *testing.T) {
	from := &schema.Column{
		Name:  "id",
		Type:  &schema.ColumnType{Type: &schema.IntegerType{T: TypeBigInt}},
		Attrs: []schema.Attr{&AutoRandom{Bits: 5}},
	}
	to := &schema.Column{
		Name:  from.Name,
		Type:  &schema.ColumnType{Type: &schema.IntegerType{T: TypeBigInt}},
		Attrs: []schema.Attr{&AutoRandom{Bits: 8}},
	}
	table := schema.NewTable("users").AddColumns(from)

	d := &tdiff{diff{conn: &conn{V: "5.7.25-TiDB-v6.6.0"}}}
	change, err := d.ColumnChange(table, from, to, nil)
	require.NoError(t, err)
	require.IsType(t, &schema.ModifyColumn{}, change)

	planner := &tplanApply{planApply{conn: &conn{V: "5.7.25-TiDB-v6.6.0"}}}
	plan, err := planner.PlanChanges(context.Background(), "test", []schema.Change{
		&schema.ModifyTable{T: table, Changes: []schema.Change{change}},
	})
	require.NoError(t, err)
	require.Equal(t, "ALTER TABLE `users` MODIFY COLUMN `id` bigint NOT NULL /*T![auto_rand] AUTO_RANDOM(8) */", plan.Changes[0].Cmd)
	require.False(t, plan.Reversible)
	require.Nil(t, plan.Changes[0].Reverse)
}

func TestTiDBAutoRandomShardBitsDecreaseRejects(t *testing.T) {
	from := &schema.Column{
		Name:  "id",
		Type:  &schema.ColumnType{Type: &schema.IntegerType{T: TypeBigInt}},
		Attrs: []schema.Attr{&AutoRandom{Bits: 8}},
	}
	to := &schema.Column{
		Name:  from.Name,
		Type:  &schema.ColumnType{Type: &schema.IntegerType{T: TypeBigInt}},
		Attrs: []schema.Attr{&AutoRandom{Bits: 5}},
	}
	table := schema.NewTable("users").AddColumns(from)

	d := &tdiff{diff{conn: &conn{V: "5.7.25-TiDB-v6.6.0"}}}
	_, err := d.ColumnChange(table, from, to, nil)
	require.EqualError(t, err, `TiDB does not support altering AUTO_RANDOM on column "id"; recreate the column or table`)
}

func TestTiDBAutoRandomAddColumnRejects(t *testing.T) {
	d := &tplanApply{planApply{conn: &conn{V: "5.7.25-TiDB-v6.6.0"}}}
	table := schema.NewTable("users").AddColumns(schema.NewIntColumn("existing", "int"))
	column := &schema.Column{
		Name:  "id",
		Type:  &schema.ColumnType{Type: &schema.IntegerType{T: "bigint"}},
		Attrs: []schema.Attr{&AutoRandom{}},
	}
	_, err := d.PlanChanges(context.Background(), "test", []schema.Change{
		&schema.ModifyTable{T: table, Changes: []schema.Change{&schema.AddColumn{C: column}}},
	})
	require.EqualError(t, err, `alter table "users": TiDB does not support adding AUTO_RANDOM column "id"; create it with the table`)
}

func TestTiDBAutoRandomRejectsTwoParameters(t *testing.T) {
	table := &schema.Table{
		Attrs: []schema.Attr{&CreateStmt{S: "CREATE TABLE `users` (\n  `id` bigint NOT NULL /*T![auto_rand] AUTO_RANDOM(5, 54) */\n)"}},
	}
	c := &schema.Column{Name: "id", Type: &schema.ColumnType{Type: &schema.IntegerType{T: "bigint"}}}
	err := (&tinspect{}).patchColumn(context.Background(), table, c)
	require.EqualError(t, err, `unsupported TiDB AUTO_RANDOM(S, R) on column "id": only AUTO_RANDOM(S) is supported`)
}

func TestTiDBAutoRandomMetadataNoise(t *testing.T) {
	table := &schema.Table{Attrs: []schema.Attr{&CreateStmt{S: "CREATE TABLE `users` (\n  `id` bigint NOT NULL /*T![auto_rand] AUTO_RANDOM(16) */ PRIMARY KEY /*T![clustered_index] CLUSTERED */\n) /*T! PRE_SPLIT_REGIONS=2 */"}}}
	column := &schema.Column{Name: "id", Type: &schema.ColumnType{Type: &schema.IntegerType{T: "bigint"}}}
	err := (&tinspect{}).patchColumn(context.Background(), table, column)
	require.EqualError(t, err, `invalid TiDB AUTO_RANDOM bit count 16 on column "id": want 1..15`)

	table.Attrs[0] = &CreateStmt{S: "CREATE TABLE `users` (\n  `id` bigint NOT NULL /*T![clustered_index] CLUSTERED */ PRIMARY KEY\n) /*T! PRE_SPLIT_REGIONS=2 */"}
	require.NoError(t, (&tinspect{}).patchColumn(context.Background(), table, column))
	require.False(t, sqlx.Has(column.Attrs, &AutoRandom{}))
}

func TestTiDBAutoRandomBitsEqual(t *testing.T) {
	require.True(t, autoRandomBitsEqual(0, 5))
	require.True(t, autoRandomBitsEqual(5, 0))
	require.False(t, autoRandomBitsEqual(4, 5))
}

func TestMySQLFamilyAutoRandomPlanRejects(t *testing.T) {
	for _, version := range []string{"8.0.36", "10.6.0-MariaDB"} {
		t.Run(version, func(t *testing.T) {
			d := &planApply{conn: &conn{V: mysqlversion.V(version)}}
			table := schema.NewTable("users").AddColumns(
				&schema.Column{
					Name:  "id",
					Type:  &schema.ColumnType{Type: &schema.IntegerType{T: "bigint"}},
					Attrs: []schema.Attr{&AutoRandom{}},
				},
			)
			_, err := d.PlanChanges(context.Background(), "test", []schema.Change{&schema.AddTable{T: table}})
			require.EqualError(t, err, `create table "users": AUTO_RANDOM is only supported by TiDB`)
		})
	}
}
