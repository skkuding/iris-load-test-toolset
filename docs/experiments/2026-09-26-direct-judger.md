# 1차 (2026-09-26): 직접 Judger 사다리와 재부팅 반복

원문: [1차 결과](https://app.notion.com/p/3e7ef9cff5458059b868e438f5b1f46a),
[1차 테스트 방법](https://app.notion.com/p/3e7ef9cff5458053bed7c60f0fea293d).
영문 비교 문서는 [WAVE1-COMPARISON.md](../WAVE1-COMPARISON.md)입니다.

## 질문

파일럿의 "동시성이 높아질수록 재현성이 떨어진다"는 현상이 Iris·RabbitMQ·
Kubernetes 없이 Judger만으로도 재현되는가?

## 조건

- 호스트: server8(`codedang8`). 클러스터에서 분리된 벤치마크 전용 호스트이며
  `k3s-agent`는 비활성입니다. Xeon E5-2620 v4 2소켓, 16코어/32스레드,
  Ubuntu 24.04, cgroup v2, Docker 29.
- CPU 정책: governor `performance`, turbo on, SMT on. Ansible
  `iris_benchmark_host` 역할로 프로비저닝했습니다.
- 슈트: 직접 Judger(`judger`), `production-compat` 모드. 워커마다
  `docker run --rm --privileged --cgroupns=host` 컨테이너 하나.
- 이미지: `images/judger-bench.Dockerfile`로 만든 로컬 이미지 ID. Judger
  `v1.0.0-alpha.4`(`sha256:2c9a4da8…`)를 포함합니다.
- 워크로드: `fixtures/cpp-runtime-v1`. 결정적 C++ 반복 연산에 약 44 MiB
  워킹셋을 더해 정답 제출의 메모리 사용을 모사합니다. 매 표본의 출력 해시를
  기대값과 비교합니다.
- 설정: `config/wave1-sweep.json`의 `ladder-1`~`ladder-30`. 워커당 50회,
  1워커만 파일럿 실험 A에 맞춰 150회입니다. 표본 수는 150·100·200·400·800·1500.

## 결과 (CPU 시간, ms)

| 워커 | 표본 | 중앙값 | CV | p99 | max | real 중앙값 |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 150 | 380 | 0.60% | 386 | 387 | 414 |
| 2 | 100 | 385 | 0.86% | 397 | 397 | 421 |
| 4 | 200 | 394 | 2.52% | 421 | 438 | 431 |
| 8 | 400 | 434 | 1.93% | 457 | 470 | 471 |
| 16 | 800 | 495 | 2.21% | 531 | 556 | 528 |
| 30 | 1500 | 564 | 3.80% | 574 | 576 | 606 |

메모리는 모든 단계에서 약 44.7 MiB였습니다.

### 재부팅 10회 반복

회차마다 재부팅 → warmup(1워커 30회, 폐기) → 전체 사다리를 실행했습니다.

| 워커 | CPU 중앙값 평균 | 최소~최대 | 회차 간 편차 |
| ---: | ---: | ---: | ---: |
| 1 | 380.7 | 380.0~381.0 | 0.3% |
| 2 | 383.9 | 382.0~386.0 | 1.0% |
| 4 | 393.1 | 390.5~395.5 | 1.3% |
| 8 | 433.1 | 432.0~435.0 | 0.7% |
| 16 | 495.3 | 495.0~496.0 | 0.2% |
| 30 | 564.5 | 564.0~565.0 | 0.2% |

## 결론

- 워커가 늘수록 중앙값이 오르고 분포가 넓어지는 파일럿의 경향이 재현됩니다.
- 30워커 증가 폭(+48%)은 파일럿(+111%)보다 작습니다. 측정 구간에 Iris,
  RabbitMQ, 컴파일, Kubernetes 스케줄링이 없기 때문입니다.
- 호스트·커널·이미지를 고정하면 재부팅 간 재현성은 매우 높습니다.

## 재현

공통 준비는 [재현 가이드](../REPRODUCTION.md)를 따르세요. 단계별 명령은
다음과 같습니다.

```bash
# 1) 워크로드 바이너리 (Ubuntu 24.04 g++)
g++ -DONLINE_JUDGE -O2 -Wall -std=c++14 fixtures/cpp-runtime-v1/source.cpp -lm -o /tmp/cpp-runtime-v1

# 2) codedang8에서 judger-bench 이미지를 만들고 ID를 확인
docker build -f images/judger-bench.Dockerfile -t judger-bench:alpha.4 .
docker image inspect --format '{{.Id}}' judger-bench:alpha.4

# 3) 단계마다 실행 → 수집 (N = 1 2 4 8 16 30, 단계 사이 load1 < 0.7 대기)
bin/iris-benchctl run --config config/wave1-sweep.json --host codedang8 \
  --profile ladder-$N --run-id iris-<YYYYMMDD>-wave-$N \
  --production-compat --worker-timeout 5m \
  --iris-digest sha256:39b743d6e1ffb1efaa18f42b509c2017b91c11976971c862beafda2ae1521250 \
  --judger-digest 2c9a4da817e06f49daabe6b437f81d10f78b42be517d2e345ab4f868a4189103 \
  --benchmark-image sha256:<image-id> \
  --local-agent bin/iris-bench-agent --local-bench-binary /tmp/cpp-runtime-v1 \
  --fixture cpp-runtime-v1=fixtures/cpp-runtime-v1/input.txt \
  --expected-output cpp-runtime-v1=fixtures/cpp-runtime-v1/output.txt \
  --cgroup-parent /sys/fs/cgroup/user.slice/user-<uid>.slice/user@<uid>.service \
  --ssh-control-path ~/.ssh/sockets/codedang8.sock
bin/iris-benchctl collect --config config/wave1-sweep.json --host codedang8 \
  --run iris-<YYYYMMDD>-wave-$N --ssh-control-path ~/.ssh/sockets/codedang8.sock
bin/iris-benchctl analyze --run runs/iris-<YYYYMMDD>-wave-$N

# 4) 도표
uv run analysis/plot-ladder.py --run 1=runs/<run-1> --run 2=runs/<run-2> \
  --run 4=runs/<run-4> --run 8=runs/<run-8> --run 16=runs/<run-16> \
  --run 30=runs/<run-30> --out wave1-direct-sweep.png
```

- `--iris-digest`는 직접 슈트에서도 계획 봉인에 필요합니다. 위 값은 당시
  `codedang-iris:stage` digest이며, 새 실험이면 다시 확인하세요.
- `--qualification-report`로 Ansible 자격 리포트를 넣지 않으면 결과는
  비교 불가로 표시되지만 수집은 됩니다.
- 재부팅 반복은 [2차 문서](2026-09-27-full-iris.md#재현)의 회차 스크립트를
  직접 슈트 명령으로 바꿔 쓰면 됩니다. 1차 회차 스크립트 자체는 커밋되어 있지
  않습니다.

## 산출물

- 사다리 번들: `runs/iris-20260926-wave2-{1,2,4,8,16,30}`
- 재부팅 반복 번들: `runs/reboot-study/iris-20260928-rb{01..10}-{1,2,4,8,16,30}`
- 도표: [images/wave1-direct-sweep.png](../images/wave1-direct-sweep.png)

번들은 실험을 수행한 운영자 머신에만 있습니다.
