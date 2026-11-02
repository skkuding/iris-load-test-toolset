# 4차 (2026-10-29~31): 분산 원인 규명

원문: [4차 결과](https://app.notion.com/p/408ef9cff5458140b03fe73e493feeac),
[측정 설계와 재현 절차](https://app.notion.com/p/40aef9cff545812daa86df49c9743871),
[쉽게 풀어 쓴 해설](https://app.notion.com/p/408ef9cff54581179fd3cca3b59b4299),
[메모리 워크로드 터보 off 재측정](https://app.notion.com/p/408ef9cff54581aa9de8c7bea7d071bf).

## 질문

워커(pod) 수가 늘면 왜 같은 코드의 채점 시간이 길어지고 흔들리는가? 1~3차는
경향만 확인했고 원인은 후보로 남아 있었습니다.

## 가설과 핵심 도구

| 가설 | 예측 | 반증 조건 |
| --- | --- | --- |
| H1 터보 클럭 | 부하가 늘면 클럭이 내려가고 cycles는 그대로 | cycles가 변하거나, 클럭을 고정해도 시간이 변함 |
| H2 SMT 공유 | 형제 스레드를 공유하면 cycles 증가(IPC 하락) | 공유해도 cycles 불변 |
| H3 캐시·대역폭·NUMA | 작업 집합이 L3를 넘거나 메모리 채널을 공유하면 cycles와 시간이 함께 증가 | 코어를 고정해도 분산이 남지 않음 |
| H4 소프트웨어 | cgroup 위치나 컴파일 겹침이 분포를 바꿈 | 조건만 바꿨을 때 차이 없음 |

시간을 클럭 요인과 일 요인으로 나눕니다.

```text
cpuTime ≈ cycles / f_eff,   f_eff = cycles / ref-cycles × 2.1 GHz,   IPC = instructions / cycles
```

측정기 `meas.c`는 libjudger와 같은 방식(`fork → exec → wait4`의 `ru_utime`)으로
`cpuTime`을 재고, 같은 프로세스에 `perf_event_open`으로 cycles, instructions,
ref-cycles, migrations, 마지막 실행 CPU를 붙입니다.

## 조건

- 호스트: server8. Iris 런타임 이미지(`codedang-iris:go1.27.3`) 컨테이너,
  `--network none`. 워커는 pod가 아니라 프로세스입니다.
- 부하: 절대 시각 barrier로 동시 출발, closed loop, 워커당 30회, 정상 상태
  구간만 집계. 단계 전 `procs_running ≤ 3`과 8초 쿨다운.
- 워크로드(실행당 일의 양 고정):

| 이름 | 내용 | 겨냥한 병목 |
| --- | --- | --- |
| fixture | 1·2차의 `cpp-runtime-v1` | ALU·레지스터(계산 위주) |
| `chase6` (L3 6MiB) | 6 MiB 포인터 추적 | L3 용량 |
| `stream44` (DRAM 대역폭) | 44 MiB 순차 읽기 | 메모리 대역폭 |
| `chase44` (DRAM 지연) | 44 MiB 포인터 추적 | 메모리 지연 |

- 배치 시나리오: `free`(고정 없음), `core`(물리 코어당 1개), `smt`(형제
  스레드에 몰아넣기), `node1`(소켓 1만), `mix`(매 실행 g++ 컴파일 포함).

## 결과: 워커 수별 cpuTime (중앙값 ms / CV)

| 워커 | fixture | L3 6MiB | DRAM 대역폭 | DRAM 지연 |
| ---: | ---: | ---: | ---: | ---: |
| 1 | 381 / 0.7% | 361 / 0.6% | 377 / 0.8% | 375 / 0.9% |
| 4 | 389 / 2.3% | 390 / 3.6% | 430 / 15% | 392 / 1.5% |
| 8 | 435 / 1.8% | 553 / 51% (p95 1439) | 548 / 29% | 407 / 1.3% |
| 16 | 497 / 0.6% | 1742 / 0.7% | 971 / 38% | 429 / 4.7% |
| 30 | 569 / 2.7% | 1916 / 2.4% | 1850 / 36% (p95 3675) | 524 / 9.1% |

fixture `free`의 클럭 분해:

| 워커 | 중앙값 | 클럭 | cycles | IPC |
| ---: | ---: | ---: | ---: | ---: |
| 1 | 381 | 3.00 GHz | 1.139 G | 2.11 |
| 8 | 435 | 2.68 | 1.140 | 2.11 |
| 16 | 497 | 2.30 | 1.139 | 2.11 |
| 24 | 556 | 2.30 | 1.274 | 1.88 |
| 30 | 569 | 2.30 | 1.302 | 1.84 |

## 원인

| 메커니즘 | 근거 | 영향 |
| --- | --- | --- |
| 터보 클럭 하락 | 16워커까지 cycles 불변, 클럭만 3.0→2.3 GHz. 소켓 단위로 결정됨 | 최대 +30% |
| SMT 공유 | 16워커 초과 시 클럭 고정, cycles +14%. 형제 스레드 2개만으로도 재현 | +14% |
| L3 용량 경합 | 소켓당 6 MiB × 4 > 20 MiB. 코어 고정에서도 CV 52% | CV 51%, 최악 약 4배 |
| 메모리 대역폭 | socket0 DIMM 3개(실효 2채널), socket1 DIMM 1개. 8워커에서 528 vs 934 ms | CV 15~38% |
| 원격 NUMA | 1워커, numactl로 CPU·메모리 노드 교차 | 지연 +72%, 대역폭 +56%, 계산 위주 0% |
| 컴파일 겹침 | 바쁜 코어 1개와 같은 효과 | 새 메커니즘 없음 |
| cgroup 위치 | alpha.4와 위임 cgroup 수정본이 1/8/16/30워커에서 같은 값 | 영향 없음 |

## 완화책 실측

| 방안 | 결과 |
| --- | --- |
| 터보 끄기 | 1~16워커 모두 544 ms, CV 0.4~0.7%. 24·30·32워커는 SMT 때문에 +14.8%와 24워커 이봉 분포(CV 5.4%)가 남음. 메모리 위주 분산은 그대로 |
| 최대 클럭 2.3 GHz 상한 | 효과 없음(Broadwell은 기본 클럭 초과 요청을 하드웨어 터보로 처리) |
| 물리 코어당 1개 고정 | SMT 요인(+14%)만 제거 |
| Intel CAT(워커당 5 way) | L3 6MiB CV 52% → 9%, 대신 중앙값 487 → 1162 ms |
| CAT을 대역폭 워크로드에 적용 | 효과 없음(이 세대에는 MBA 없음) |
| cycles 기준 시간 | 계산 위주의 클럭 요인 제거, 메모리 위주에는 무효 |

메모리 워크로드는 터보를 꺼도 분산이 줄지 않습니다(L3 CV 51% → 48%, 대역폭
29% → 28%). 대역폭 분산은 잡음이 아니라 소켓 간 간극입니다(소켓 안 CV 1~6%,
소켓 사이 1.7배).

## 결론과 운영 권고

채점 시간은 pod에 준 CPU 수가 아니라 그 순간 같은 소켓과 메모리 길을 함께 쓰는
작업량으로 정해집니다. 권고 순서는 다음과 같습니다.

1. 모든 노드의 클럭 정책을 같게, 가능하면 터보 off. 계산 위주 코드에만 효과가
   있습니다.
2. 채점을 NUMA 로컬에 두고, 메모리가 없는 소켓은 피합니다.
3. 물리 코어당 채점 1개(CPU Manager `static` + `full-pcpus-only`, 또는 SMT off).
4. 소켓당 동시 채점 수 제한 또는 채점 전용 노드.
5. 보조: 제한 시간 ±10% 결과는 재채점하고, cycles/instructions를 함께 기록.

위임 cgroup(수정 libjudger)은 분산을 직접 줄이지 않지만, 2·3번이 제출 코드에
적용되려면 필요합니다.

## 부수 발견

- libjudger 박스 이름 `box-<초>-<마이크로초>`가 동시 실행에서 충돌해 30워커에서
  간헐 실패했습니다(alpha.4 815회 중 9회). 이름에 PID를 붙여 900회 중 0회를
  확인했습니다.
- 운영 노드는 swap이 켜져 있고 박스 `memory.swap.max=max`여서 MLE가 판정되지
  않을 수 있습니다. 수정 libjudger에서 `memory.swap.max=0`으로 처리했습니다.

## 재현

4차 측정 코드는 **이 레포에 없습니다.** 실험자의 로컬 작업 디렉터리
`iris-variance-exp/`와 server8의 `~/iris-variance-exp`에 있습니다.

| 파일 | 역할 |
| --- | --- |
| `meas.c` | 측정기 |
| `wl.c` | 메모리 워크로드(`chase`, `stream`) |
| `run-ladder.sh` | 워커 단계 실행. 시나리오 `free`/`core`/`smt`/`node1`/`mix`, 환경 변수 `WL`, `WL_INPUT`, `TAG`, `JUDGER` |
| `freqctl.sh` | 클럭 정책 `save`/`noturbo`/`cap`/`restore` |
| `run-memory-noturbo.sh` | 터보 off 메모리 전체 측정(복원 포함) |
| `analyze.py`, `analyze-memory.py`, `plot.py`, `plot-memory.py` | 분석과 도표 |
| `METHOD.md`, `FINDINGS.md`, `LABELS.md`, `MEMORY-TURBO-OFF.md` | 방법, 결과, 라벨 사전 |
| `r1`~`r7`, `numa-1w*.csv` | 원자료 |

```bash
# 측정기와 워크로드 빌드 (Iris 런타임 이미지 안)
docker run --rm --privileged --network none -v $D:$D -w $D --entrypoint bash \
  codedang-iris:go1.27.3 -c \
  'gcc -O2 -Wall -o meas meas.c && g++ -DONLINE_JUDGE -O2 -Wall -std=c++14 source.cpp -lm -o bench && gcc -O2 -o wl wl.c'

# fixture 사다리 (워커 1 → 32)
./run-ladder.sh r1 free 30 1 2 4 8 12 16 24 30 32

# L3 워크로드
WL="chase 6" WL_INPUT=in-chase6.txt TAG=chase6 ./run-ladder.sh r2 free 30 1 4 8 16 30

# 터보 off 메모리 전체 (복원 포함)
./run-memory-noturbo.sh

# 분석과 도표
python3 analyze.py r1
python3 analyze-memory.py
python3 plot.py && python3 plot-memory.py
```

주의:

- 터보, 최대 클럭, resctrl을 바꾸는 블록은 같은 셸에서 `trap`으로 복원하고,
  끝난 뒤 `no_turbo=0`, 최대 클럭 3.0 GHz, resctrl 마운트 없음, 남은
  `/sandbox-*` 0개를 확인했습니다.
- 한 번은 `numactl`이 없는 컨테이너에서 NUMA 측정을 시작해 실패했고
  `no_turbo=1`이 남았습니다. 호스트 설정을 바꾸는 실험은 반드시 복원을 확인하세요.
- 이 코드를 레포로 옮기면 재현성이 좋아집니다. [다음 실험 계획](../NEXT-EXPERIMENT.md)의
  후보로 남겨 두었습니다.

## 한계

- server8 한 대의 결과입니다. CPU는 운영 노드와 같지만 DIMM 구성은 노드마다
  다릅니다.
- 워크로드는 메커니즘 분리용 합성 코드입니다. 실제 제출은 이들이 섞여 있습니다.
- 운영 노드의 NUMA 영향은 server8 수치를 대입한 추론입니다.
- `ru_utime`은 틱 기반이라 1 ms 근처에서 양자화됩니다.
