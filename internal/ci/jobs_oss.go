// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

//go:build !ent

package main

//go:generate go run . -flavor Community -suffix oss

func init() {
	data.GoVersions = goVersions{"1.26.4"}
	data.GlobalEnv = []struct{ K, V string }{
		{K: "ATLAS_NO_UPGRADE_SUGGESTIONS", V: "1"},
	}
	data.Jobs = append(jobs,
		Job{
			Version: "tidb5",
			Image:   "pingcap/tidb:v5.4.0",
			Regex:   "TiDB",
			Ports:   []string{"4309:4000"},
		},
		Job{
			Version: "tidb6",
			Image:   "pingcap/tidb:v6.6.0",
			Regex:   "TiDB",
			Ports:   []string{"4310:4000"},
		},
		Job{
			Version: "tidb8",
			Image:   "pingcap/tidb:v8.5.3",
			Regex:   "TiDB",
			Ports:   []string{"4311:4000"},
		},
		Job{
			Version: "cockroach",
			Regex:   "Cockroach",
			Steps: []Step{{
				Name: "Start CockroachDB",
				Run: `docker run --detach --rm --name atlas-cockroach --publish 26257:26257 cockroachdb/cockroach:v22.1.0 start-single-node --insecure
				for i in $(seq 1 60); do
					if docker exec atlas-cockroach /cockroach/cockroach sql --insecure --execute "SELECT 1" >/dev/null 2>&1; then
						exit 0
					fi
					sleep 1
				done
				docker logs atlas-cockroach
				exit 1`,
			}},
		},
	)
}
