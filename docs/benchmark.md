# Synthetic benchmark — 2026-09-16

The schema 2 search-row lookup reduced default-search p95 from **776.079 ms to 110.892 ms** on the same synthetic corpus. The proposed 500 ms direct-store search target passes after this change. These measurements do not establish private-LAN HTTP/TLS latency or cold-cache performance.

## Hardware and corpus

- Reference server: KVM VM, four vCPUs on Intel Core i5-12500T; Linux 6.1.0-52-amd64; local ext4 on /dev/vda1.
- 10,000 documents, exactly 1,073,741,824 source-content bytes. Deterministic technical-word corpus, 100 projects, distinct titles/identifiers, pseudorandom numeric text.
- Single concurrent caller; 200 measurements per operation. Search/list return their first page, at limit 50.
- Warm OS cache following corpus ingestion (and, for schema 2, migration). No cache eviction was performed. Full-get includes the durable inactivity-clock update.
- Database was isolated in a private benchmark scratch directory, represented below by `$BENCHMARK_DIR`. No production documents were used.
- Corpus construction took 269.097 seconds using source commit f1630be11d95073212d130e6daa21a061af6c572. This revision already omits redundant FTS deletion during create.

## Results

| Operation | Schema 1 p50 / p95 / maximum (ms) | Schema 2 p50 / p95 / maximum (ms) |
|---|---:|---:|
| Default search | 760.748 / 776.079 / 804.911 | 106.308 / 110.892 / 137.241 |
| Metadata list | 25.143 / 27.962 / 33.496 | 24.165 / 25.672 / 30.263 |
| Full get | 7.740 / 9.175 / 14.592 | 7.641 / 10.689 / 13.367 |

First measured search was 1,011.943 ms before and 105.430 ms after. This is not a cold-cache measurement.

The schema 2 binary was built from the source changes in the commit containing this report, whose parent is f1630be11d95073212d130e6daa21a061af6c572. Rerun with the published release commit to attribute subsequent measurements independently.

Native SQLite diagnosis on this corpus: retrieving 10,000 matching FTS rowids took 9.450 ms; retrieving their unindexed document-ID column took 228.830 ms. The unindexed column sits beside large FTS content. An ordinary rowid-to-ID table removes that content access from candidate selection without capping exact/FTS matches or changing pagination.

## Reproduction and retained evidence

Build cmd/jocasta-bench. Choose a new private scratch directory as `$BENCHMARK_DIR`. Initial creation:

    jocasta-bench --db NEW_DATABASE --documents 10000 --bytes 1073741824 --samples 200

Measure the same synthetic corpus without rebuilding it:

    jocasta-bench --db EXISTING_BENCHMARK_DATABASE --documents 10000 --bytes 1073741824 --samples 200 --measure-only

The utility refuses non-benchmark documents in measure-only mode and reserves 10 GiB of free disk space.

Private raw reports were retained in the benchmark scratch directory:

- $BENCHMARK_DIR/benchmark-f1630be.json
- $BENCHMARK_DIR/benchmark-schema2.json

Corpus database: $BENCHMARK_DIR/benchmark-f1630be.db. The interrupted initial 4e1891998e3463002f3b44cd3f5370c108c5df87 experiment remains separately at benchmark-4e18919.db (1,222 documents); it is not part of these results.

## Migration and recovery evidence

Schema 1 to 2 migration automatically created a SQLite-aware online backup at benchmark-f1630be.db.pre-v2.TIMESTAMP.db before transactionally creating the rowid lookup. The actual f1630be benchmark binary was then run against schema 2 and refused it with:

    unsupported database schema 2; binary supports 1

Automated migration coverage verifies backup schema 1, new schema 2, stable IDs, historical bytes, move aliases, exact idempotency receipts, current search, absence of stale search, and reconstruction of the lookup during reindex. A separate test verifies that an unsupported newer schema leaves database bytes unchanged. Full tests and backend race tests passed.

Deployment must retain the pre-migration backup. Downgrading to a schema 1 binary requires an operator-led restore with an explicit recovery timestamp and loss window; do not silently replace a database containing newer writes.
