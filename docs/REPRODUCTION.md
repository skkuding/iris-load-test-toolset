# 재현 가이드

과거 실험을 다시 돌리거나 새 실험을 같은 방식으로 실행할 때 따르는 공통
절차입니다. 실험별 명령은 각 [실험 문서](experiments/README.md)의 "재현" 절에
있습니다.

## 재현 가능 여부

| 실험 | 레포만으로 재현 | 필요한 것 |
| --- | --- | --- |
| [파일럿](experiments/2026-09-18-pilot.md) | 불가 | 비울 수 있는 클러스터 노드, 레포 밖 매니페스트·드라이버, 운영 작업 승인 |
| [1차](experiments/2026-09-26-direct-judger.md) | 가능 | `codedang8`, judger-bench 이미지, 위임 cgroup |
| [2차](experiments/2026-09-27-full-iris.md) | 가능 | 1차 조건 + 벤치마크 AWS 프로필(RDS/S3 읽기) |
| [3차](experiments/2026-10-21-go-toolchain-ab.md) | 부분 | 2차 조건 + 비교할 두 Iris 이미지 |
| [4차](experiments/2026-10-29-variance-root-cause.md) | 불가 | 레포 밖 `iris-variance-exp/` 코드 |
| [RDT](experiments/2026-10-31-rdt-monitoring.md) | 가능 | 1차 조건 + resctrl CMT/MBM 지원 |
| [CAT](experiments/2026-10-31-cat-equal-ab.md) | 가능 | `codedang8` root 권한(대화형 sudo), resctrl CAT |

## 1. 준비

### 로컬 도구

- Go 1.24 이상, Python 3, [`uv`](https://docs.astral.sh/uv/)(도표 스크립트)
- OpenSSH, `jq`, AWS CLI v2(2·3차)
- Ansible(`ansible-core`와 `ansible-galaxy collection install -r ansible/requirements.yml`),
  호스트를 다시 프로비저닝할 때만 필요

```bash
go build ./... && go vet ./... && go test ./...
go build -o bin/iris-benchctl    ./cmd/iris-benchctl
go build -o bin/iris-bench-agent ./cmd/iris-bench-agent
go build -o bin/judger-bench     ./cmd/judger-bench
```

에이전트는 벤치마크 호스트(Linux amd64)에서 실행됩니다. 로컬이 다른 OS나
아키텍처면 `GOOS=linux GOARCH=amd64 CGO_ENABLED=0`으로 빌드해 `--local-agent`에
넘기세요.

### 네트워크와 SSH

- server8에 닿는 네트워크(학교 VPN 등)가 필요합니다.
- SSH 별칭 `codedang8`이 비대화형으로 동작해야 합니다. 컨트롤러는
  `BatchMode=yes`를 씁니다.

```bash
ssh -o BatchMode=yes -o ConnectTimeout=15 codedang8 true
ssh -M -S ~/.ssh/sockets/codedang8.sock -o ControlPersist=3600 -fN codedang8
```

- 기존 소켓을 쓰려면 `--ssh-control-path ~/.ssh/sockets/codedang8.sock`을
  `run`, `collect`, `status`에 넘깁니다.

### 호스트 상태

- 벤치마크 전용 호스트는 `codedang8`(`skkuding-4f-4`) 하나뿐이며 설정
  파일의 허용 목록에도 이것만 있습니다.
- 다른 실험이 돌고 있지 않은지 확인합니다.

```bash
ssh codedang8 'cat /proc/loadavg; docker ps --format "{{.Names}}"; ls -d /sys/fs/cgroup/sandbox-* 2>/dev/null; mount | grep resctrl'
ssh codedang8 'cat /sys/devices/system/cpu/intel_pstate/no_turbo; cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor'
```

- 기준 상태는 load1 < 0.7, 실행 중인 벤치마크 컨테이너 없음, `/sandbox-*` 없음,
  `no_turbo=0`, governor `performance`입니다. 컨트롤러는 load1이
  `limits.maxLoad1`(기본 1.0)을 넘으면 실행을 거부합니다.
- 위임 cgroup 부모는 `/sys/fs/cgroup/user.slice/user-<uid>.slice/user@<uid>.service`
  입니다. `ssh codedang8 id -u`로 uid를 확인하세요. 과거 실험은 uid 1001을
  썼습니다.

### 이미지와 digest

| 항목 | 과거 값 | 확인 방법 |
| --- | --- | --- |
| Judger alpha.4 | `2c9a4da817e06f49daabe6b437f81d10f78b42be517d2e345ab4f868a4189103` | `images/judger-bench.Dockerfile`의 `JUDGER_AMD64_SHA256` |
| Iris (stage, 직접 슈트 봉인용) | `sha256:39b743d6e1ffb1efaa18f42b509c2017b91c11976971c862beafda2ae1521250` | `--resolve-image` 또는 `docker buildx imagetools inspect` |
| Iris (production, 2차) | `sha256:98adb52a1f41837def5e7d725b9052e04a8b7b48ed1bcdcaa1d66f0ff5fe41e7` | 위와 같음 |
| RabbitMQ 3.13 | `sha256:606d8c0d6b3c18d1da9afc53bc7cdb2a8d5486df91b5a9830e9e07626c9ae281` | 위와 같음 |
| judger-bench 이미지 | 빌드할 때마다 다름 | `codedang8`에서 `docker image inspect --format '{{.Id}}' judger-bench:alpha.4` |

과거 실험을 그대로 재현할 때는 과거 digest를 쓰고, 새 실험은 새로 확인한
값을 기록합니다. judger-bench 이미지는 에이전트가 원격에서 `docker run`하므로
`codedang8`에 있어야 합니다. 그곳에서 빌드하거나 `docker save`와
`docker load`로 옮기세요. 이미지 ID는 그대로 유지됩니다.

### AWS (2·3차)

`codedang-iris-benchmark-terraform`, `codedang-iris-benchmark-read` 프로필이
필요합니다. 런타임 자격 증명 파일은 단계마다 새로 만들고 실행 후 지웁니다.

```bash
set -a; source .env.example; set +a
scripts/aws/build-iris-runtime-env.sh --out /tmp/iris-runtime.env \
  --secret-profile codedang-iris-benchmark-terraform \
  --s3-profile codedang-iris-benchmark-read --yes
```

## 2. 실행 흐름

```text
plan(선택) → run → collect → analyze → 도표
```

- `plan`: 실행 계획 JSON과 SHA-256을 출력합니다. 아무것도 바꾸지 않습니다.
  `run --dry-run`도 SSH 호출 없이 계획만 보여 줍니다.
- `run`: 계획을 봉인하고 원격에서 `inspect → prepare → run-block → validate →
  cleanup → bundle`을 실행합니다. 수집은 하지 않습니다.
- `collect`: 번들을 내려받아 해시를 검증하고 `runs/<run-id>`에 씁니다. 기존
  디렉터리를 덮어쓰지 않습니다.
- `analyze`: 정상 상태 구간 기준 통계를 JSON으로 출력합니다.

run ID는 `iris-<YYYYMMDD>-<설명>` 형식을 따르고, 한 번 쓴 ID는 다시 쓰지
않습니다.

## 3. 실행 중 지킬 것

- 단계 사이에는 load1 < 0.7을 기다립니다.
- 재부팅, 터보·클럭 변경, resctrl 마운트·그룹 생성, `cpuset` 위임 변경은
  호스트 상태를 바꿉니다. 실행 전에 사용자 승인을 받고, 같은 스크립트 안에서
  `trap`으로 원래 값을 되돌리고, 끝난 뒤 다시 읽어 확인합니다.
- 실패한 run은 지우지 말고 원인을 기록합니다. 원격 증거는
  `/var/lib/iris-bench/<run-id>`에 남습니다.

## 4. 끝난 뒤 확인

```bash
ssh codedang8 'docker ps -a --filter label=iris-bench.run --format "{{.Names}}"; ls -d /sys/fs/cgroup/sandbox-* 2>/dev/null; mount | grep resctrl; cat /sys/devices/system/cpu/intel_pstate/no_turbo'
```

- 남은 컨테이너, `/sandbox-*`, resctrl 그룹이 없어야 하고 `no_turbo=0`이어야
  합니다.
- `/tmp/iris-runtime.env` 같은 자격 증명 파일을 지웁니다.
- 결과 요약과 run ID 목록을 실험 문서와 Notion에 남깁니다. Notion에 쓰기
  전에는 사용자 승인을 받습니다.

## 5. 결과 해석 원칙

- 모든 결과는 `production-compat`, `comparable=false`입니다. 같은 실험 안에서만
  비교합니다.
- 중앙값과 CV, p95/p99를 함께 봅니다. 단일 실행의 1 ms 차이는 해석하지
  않습니다(`ru_utime` 양자화).
- A/B 비교는 같은 조건에서 번갈아 최소 2회 이상 반복하고, 회차 간 변동보다
  큰 차이만 의미 있게 봅니다.
- CMT/MBM이 resctrl 기본 그룹 값이면 호스트 전체 값입니다. Judger 단독 값으로
  해석하지 않습니다.
