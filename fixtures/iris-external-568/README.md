# External Iris fixture: problem 568 testcase 15850

This directory contains the curated direct-suite source for problem `568`,
testcase `15850`. It is deliberately not a general solution: it accepts only
the one input shape used by this benchmark testcase, performs a fixed
deterministic CPU workload, keeps an approximately 44 MiB working set resident,
and emits the expected three output lines.

The corresponding Iris testcase objects are external to this directory and are
read from the dedicated S3 bucket at:

```text
s3://codedang-iris-benchmark-testcases/568/15850.in
s3://codedang-iris-benchmark-testcases/568/15850.out
```

`userTestcases` are intentionally omitted. This fixture is for the curated
external benchmark path only; it must not be treated as a complete submission
or as a replacement for the problem's full testcase set.
