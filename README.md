# Iris Load-Test Toolset

Codedang 채점 워커 **Iris**와 샌드박스 실행기 **Judger**의 실행 시간
(`cpuTime`, `realTime`)이 얼마나 재현 가능한지 측정하는 도구 모음입니다.
같은 코드와 입력을 여러 동시성 단계(1·2·4·8·16·30 워커)에서 반복 실행하고,
결과를 해시로 봉인한 번들로 수집·분석합니다.

- 지금까지의 실험 기록과 결론: [docs/experiments](docs/experiments/README.md)
- 실험 재현 방법: [docs/REPRODUCTION.md](docs/REPRODUCTION.md)
- 다음 실험 계획: [docs/NEXT-EXPERIMENT.md](docs/NEXT-EXPERIMENT.md)
- 전체 문서 목록: [docs/README.md](docs/README.md)
- 에이전트용 명령·실행 조건: [AGENTS.md](AGENTS.md)

## 지금까지 알게 된 것

- Iris 수를 늘리면 같은 제출의 CPU 시간이 길어집니다. 계산 위주 fixture는
  1워커 약 380ms에서 30워커 약 565ms까지 늘어납니다.
- 이 증가는 pod 수 자체가 아니라 같은 CPU 소켓, 캐시, 메모리 채널을 함께 쓰는
  작업량에서 나옵니다. 터보 클럭 하락, SMT 공유, L3·메모리 대역폭 경합, 원격
  NUMA 메모리가 원인입니다.
- 서비스 경로(Iris + RabbitMQ + RDS/S3)를 포함해도 Judger 단독과 거의 같은
  값이 나오고, 재부팅 반복 간 편차는 대부분 1% 이하입니다.
- 자세한 수치와 근거는 [실험 총정리](docs/experiments/README.md)를 보세요.

## 구성 요소

| 구성 요소 | 역할 |
| --- | --- |
| `iris-benchctl` | 로컬 컨트롤러. 실행 계획을 봉인하고 원격 호스트에서 실행·수집·분석합니다. |
| `iris-bench-agent` | 벤치마크 호스트에서 SSH로 호출되는 에이전트. 데몬이 아닙니다. |
| `judger-bench` | Judger(`libjudger.so`)로 워크로드를 반복 실행하고 표본을 NDJSON으로 남깁니다. |
| `iris-bench-rdt` | Intel RDT 카운터(CMT/MBM)를 수동으로 기록합니다. |
| `ansible/` | 벤치마크 호스트 프로비저닝과 읽기 전용 자격 검사(qualification). |
| `infra/aws/`, `scripts/aws/` | 전용 벤치마크 RDS·S3·IAM과 fixture 업로드, 런타임 자격 증명 생성. |
| `analysis/`, `scripts/*.py` | 사다리·반복·RDT·CAT 도표와 요약. |

측정 모드는 두 가지입니다.

- **직접 Judger 슈트(`judger`)**: Iris 서비스 계층 없이 Judger만 실행합니다.
- **Full Iris 외부 데이터 슈트(`iris`)**: 운영 Iris 이미지와 실행 전용
  RabbitMQ, 벤치마크 RDS/S3를 사용합니다. 운영 큐와 DB는 건드리지 않습니다.

## 빠른 시작

Go 1.24 이상이 필요합니다.

```bash
go build ./... && go vet ./... && go test ./...

go build -o bin/iris-benchctl    ./cmd/iris-benchctl
go build -o bin/iris-bench-agent ./cmd/iris-bench-agent
go build -o bin/judger-bench     ./cmd/judger-bench
```

기본 흐름은 `plan → run → collect → analyze`입니다.

```bash
bin/iris-benchctl plan    --config config/wave1-sweep.json --host codedang8 --profile ladder-1 ...
bin/iris-benchctl run     --config config/wave1-sweep.json --host codedang8 --profile ladder-1 ... --run-id <run-id>
bin/iris-benchctl collect --config config/wave1-sweep.json --host codedang8 --run <run-id>
bin/iris-benchctl analyze --run runs/<run-id>
uv run analysis/plot-ladder.py --run 1=runs/<run-1> --run 30=runs/<run-30> --out ladder.png
```

실제 실행에 필요한 이미지 digest, fixture, 호스트 조건은
[재현 가이드](docs/REPRODUCTION.md)에 실험별로 정리했습니다.

## 안전 원칙

- 운영 RDS, S3, RabbitMQ, Kubernetes, 운영 노드는 **읽기 전용**입니다. 변경은
  승인된 벤치마크 호스트(`codedang8`)와 `codedang-iris-benchmark` 리소스에만
  합니다.
- 자격 증명, `.env`, `terraform.tfvars`, `*.tfstate`, 결과 번들(`runs/`)은
  커밋하지 않습니다.
- 현재 Judger alpha.4는 제출을 호스트 루트 `/sandbox-*` cgroup에서 실행하므로
  모든 결과는 `production-compat`, `comparable=false`입니다. 다른 모집단과
  절대값을 섞어 비교하지 않습니다.
- 터보, 최대 클럭, resctrl(CAT) 같은 호스트 설정을 바꾼 실험은 같은 스크립트
  안에서 원래 값으로 되돌리고 다시 확인합니다.

## 디렉터리

```text
cmd/          iris-benchctl, iris-bench-agent, judger-bench, iris-bench-rdt
internal/     계획·오케스트레이션·격리·분석 패키지
config/       컨트롤러 설정(wave1-sweep, full-iris-external 등)과 호환성 기준
fixtures/     정제된 테스트케이스와 워크로드 소스
analysis/     도표·요약 스크립트(uv run으로 실행)
scripts/      호스트 자격 검사, AWS 도구, RDT·CAT 실험 스크립트
ansible/      벤치마크 호스트 역할
infra/aws/    전용 AWS 리소스 Terraform
results/      커밋된 작은 결과 요약(CAT A/B)
docs/         실험 기록, 재현·운영 가이드, 상세 레퍼런스
```

`runs/`와 `bin/`은 Git에서 제외됩니다. 명령, 설정, 격리, AWS의 상세 동작은
[Toolset Reference](docs/reference/TOOLSET-REFERENCE.md)(영문)를 참고하세요.

## 검증

```bash
go build ./... && go vet ./... && go test ./...
fixtures/tests/verify-fixtures.sh
scripts/aws/tests/run-tests.sh
infra/aws/bootstrap/tests/run-tests.sh
infra/aws/bootstrap/tests/validate-policies.sh
infra/aws/iris-benchmark/tests/validate-terraform.sh   # 또는 --static-only
```

이 테스트들은 가짜 `aws`/`psql`과 가짜 cgroup 파일시스템만 사용합니다. 실제
호스트나 AWS에는 접속하지 않습니다.
