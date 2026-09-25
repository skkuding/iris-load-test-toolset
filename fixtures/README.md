# Benchmark testcase fixtures

Sanitized, repository-safe testcase assets for the approved target problems
`568`, `569`, and `570`. The local files are the exact objects uploaded to the
dedicated `codedang-iris-benchmark-testcases` S3 bucket; the upload script never
transforms content.

## Layout

```text
fixtures/
├── manifest.json          # pins the exact object set, sizes, and SHA-256
├── schema/
│   └── manifest.schema.json
├── metadata.json          # sanitized problem specifications and testcase IDs
├── 568/15850.in, 15850.out
├── 569/15852.in, 15852.out
├── 570/15873.in, 15873.out
└── tests/verify-fixtures.sh
```

The layout mirrors how Iris reads testcases from S3
(`apps/iris/src/loader/s3.go`): `<problemId>/<testcaseId>.in` and
`<problemId>/<testcaseId>.out`. The manifest `s3_key` is exactly the object key.

## Sanitization and provenance

Problem specifications and testcase IDs were exported read-only from the
dedicated RDS clone. Problems 569 and 570 use their public sample rows. Problem
568 had no usable inline sample data, so `15850` contains a small testcase
derived from the cloned specification. Hidden production testcases are not
committed. All six files are uploaded unchanged with `hidden=false`.

Regenerate the manifest after an intentional fixture change:

```bash
../scripts/aws/build-fixture-manifest.sh --write
```

## Verify and upload

```bash
../scripts/aws/build-fixture-manifest.sh --check   # manifest is current
tests/verify-fixtures.sh               # hashes, text, secrets
../scripts/aws/upload-fixtures.sh --dry-run        # local hash verification
../scripts/aws/upload-fixtures.sh --bucket codedang-iris-benchmark-testcases
```

`upload-fixtures.sh` uploads each manifest object individually with a SHA-256
checksum and verifies the stored checksum with `head-object`.
