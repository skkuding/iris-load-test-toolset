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
├── 568/1.in, 1.out, ...
├── 569/...
├── 570/...
└── tests/verify-fixtures.sh
```

The layout mirrors how Iris reads testcases from S3
(`apps/iris/src/loader/s3.go`): `<problemId>/<testcaseId>.in` and
`<problemId>/<testcaseId>.out`. The manifest `s3_key` is exactly the object key.

## Sanitization and provenance

The committed fixtures are synthetic, license-compatible, and contain no
production data, credentials, or personal information. They are placeholders
for the per-problem shape; the approved plan requires deriving the final
sanitized content and hidden flags from the cloned benchmark database. Replace
the assets, then regenerate the manifest:

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
