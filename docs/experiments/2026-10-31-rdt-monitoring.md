# RDT 관측 (2026-10-31): Judger 사다리와 CMT·MBM 동시 측정

원문: [CMT·MBM 동시 관측](https://app.notion.com/p/40aef9cff54581b6a255ed325f3dded2).
영문 문서는 [RDT-MONITORING.md](../RDT-MONITORING.md)입니다. 코드는 PR #3
(`feat/rdt-monitoring`)으로 `main`에 들어왔습니다.

## 질문

직접 Judger 사다리를 다시 돌리면서, 같은 정상 상태 구간에 LLC 점유량(CMT)과
메모리 대역폭(MBM)이 어떻게 변하는지 함께 기록할 수 있는가?

## 조건

- 호스트: `codedang8`, SMT on, governor `performance`
- 부하: `cpp-runtime-v1` 직접 Judger, `config/wave1-sweep.json`의 1·2·4·8·16·30
  워커. 반복 150·50·50·50·50·50회/워커. 단계 전 load1 < 0.7 대기
- 샘플러: `cmd/iris-bench-rdt`. 권한 있는 `--network none` 컨테이너에서
  resctrl `L3_MON`을 1초 간격으로 읽고, 준비 표시가 나온 뒤 부하를 시작합니다.
  CAT·MBA·CDP는 바꾸지 않습니다. 다른 resctrl 그룹이 있으면 실행을 거부합니다.
- 요약: `analysis/summarize-rdt.py`가 run receipt의 정상 상태 구간 표본만
  사용합니다. 대역폭은 두 LLC 도메인의 합, remote는 total − local 추정치입니다.

## 결과

| 워커 | 성공/실패 | CPU 중앙값 | CPU p99 | MBM total MiB/s | MBM local | MBM remote | CMT LLC 평균 MiB |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 150/0 | 382 | 387 | 247.7 | 236.6 | 11.1 | 39.84 |
| 2 | 100/0 | 386 | 402.09 | 509.2 | 470.7 | 38.5 | 39.92 |
| 4 | 200/0 | 394 | 412 | 939.4 | 912.9 | 26.5 | 39.93 |
| 8 | 400/0 | 434 | 457 | 1,717.1 | 1,675.0 | 42.0 | 39.95 |
| 16 | 800/0 | 496 | 531.85 | 2,958.0 | 2,903.8 | 54.2 | 39.95 |
| 30 | 1500/0 | 565.5 | 575 | 4,879.5 | 4,771.3 | 108.2 | 39.95 |

## 해석과 한계

- CPU 중앙값은 1차와 같은 경향으로 올랐습니다(+48%).
- MBM total도 함께 올랐지만, 이는 호스트 전체의 동반 증가입니다. Judger 단독
  소비량이나 지연 원인의 증거가 아닙니다.
- CMT는 두 도메인 합 약 40 MiB(각 20 MiB, L3 전체)로 일정했습니다. 기본
  그룹의 모든 태스크가 포함되므로 Judger 작업 집합 크기로 해석하지 않습니다.
- 단계마다 한 번씩만 실행했고, 자격 리포트가 없어 `comparable=false`입니다.

## 재현

```bash
# 레포 루트에서 (스크립트가 ./iris-benchctl 과 ./iris-bench-agent-linux-amd64 를 사용)
go build -o iris-benchctl ./cmd/iris-benchctl
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o iris-bench-agent-linux-amd64 ./cmd/iris-bench-agent
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o iris-bench-rdt-linux-amd64 ./cmd/iris-bench-rdt

# codedang8에 입력 배치
ssh codedang8 mkdir -p /tmp/iris-rdt-20261031
scp iris-bench-rdt-linux-amd64 codedang8:/tmp/iris-rdt-20261031/
scp <cpp-runtime-v1 workload> codedang8:/tmp/iris-rdt-20261031/workload   # SHA-256 71100d72… 확인

bash scripts/run-rdt-judger-ladder.sh

# 도표 (summary와 analyze JSON이 같은 디렉터리에 있어야 함)
for n in 1 2 4 8 16 30; do
  ./iris-benchctl analyze --run runs/iris-20261031-rdt-$n > runs/rdt-20261031/iris-20261031-rdt-$n.analysis.json
done
python3 analysis/plot-rdt.py --results runs/rdt-20261031 --output runs/rdt-20261031/rdt-ladder.svg
```

`scripts/run-rdt-judger-ladder.sh`는 실험 기록입니다. 다음 값이 고정되어
있으므로 다시 쓸 때 먼저 확인하고 바꾸세요.

- 호스트 `codedang8`, cgroup 부모 `user-1001`의 `user@1001.service`
- SSH control path `/private/tmp/iris-rdt-ssh.sock`(macOS 경로)
- 벤치마크 이미지 ID, Iris·Judger digest, 워크로드 SHA-256, run ID 날짜

스크립트는 위임 cgroup에 `cpuset`이 없으면 잠시 켜고, 끝나면 원래대로 돌립니다.
`analysis/plot-rdt.py`는 워커 목록(1·2·4·8·16·30)과 run 이름이 코드에 고정되어
있습니다.

## 산출물

- 번들: `runs/iris-20261031-rdt-{1,2,4,8,16,30}/`
- RDT 원본 NDJSON, 요약 JSON, 분석 JSON, 도표: `runs/rdt-20261031/`
