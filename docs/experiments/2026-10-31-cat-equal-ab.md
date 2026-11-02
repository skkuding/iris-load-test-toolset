# CAT A/B (2026-10-31): L3 균등 할당 예비 실험

원문: [CAT 균등 할당 예비 A/B](https://app.notion.com/p/40aef9cff54581c1b157cc9b48214af9).
영문 절차는 [CAT-DIRECT-AB.md](../CAT-DIRECT-AB.md), 결과 보고서와 도표는
[results/cat-direct-20261031](../../results/cat-direct-20261031/report.md)에 있습니다.
코드는 PR #4(`feat/cat-equal-ways`)로 `main`에 들어왔습니다.

## 질문

워커마다 L3 캐시 way를 겹치지 않게 같은 크기로 나눠 주면(Intel CAT) 채점
시간이나 분산이 나아지는가?

## 조건

- 호스트 `codedang8`. LLC 도메인 2개, 각 20 way(`cbm_mask=fffff`), CLOSID 16개.
  공유 표시된 상위 2 way(`c0000`)를 빼고 18 way를 사용합니다.
- 컨테이너 없이 호스트에서 `judger-bench → libjudger.so`를 직접 실행합니다.
  워커의 절반씩을 각 소켓에 두고 `numactl --membind`로 로컬 메모리를 씁니다.
  30워커는 SMT 형제도 씁니다.
- 대조군(`base`)과 CAT군(`equal`)은 CPU 고정, 입력, 바이너리, libjudger가
  같습니다. 워커당 50회, 단계마다 A/B 한 쌍.

| 워커 | CAT 클래스 | 워커당 way (도메인당) |
| ---: | ---: | ---: |
| 8 | 4 | 4 |
| 16 | 8 | 2 |
| 30 | 15 | 1 |

## 결과

| 워커 | CPU 중앙값 대조군→CAT (ms) | 변화 | CPU p99 | real 중앙값 |
| ---: | ---: | ---: | ---: | ---: |
| 8 | 434→437 | +0.69% | 448→453.19 | 469→475 |
| 16 | 497→498 | +0.20% | 503→505 | 530→531 |
| 30 | 570→571 | +0.18% | 578→579 | 620→622 |

| 워커 | CMT LLC 평균 MiB | MBM total MiB/s |
| ---: | ---: | ---: |
| 8 | 39.81→20.29 | 1637.49→1620.24 |
| 16 | 39.83→17.57 | 2874.53→2922.30 |
| 30 | 39.77→22.67 | 4655.60→5335.65 |

총 5,400회 모두 성공했고 기대 출력이 일치했습니다.

## 결론

- 이 계산 위주 fixture에서는 CAT로 CPU 시간이 개선되지 않았습니다.
  `cpp-runtime-v1`은 44 MiB를 시작할 때 한 번만 건드리는 ALU 중심 코드라
  캐시 간섭 완화 이득이 작습니다.
- 대조군의 CMT/MBM은 root 그룹(호스트 전체)이고 CAT군은 할당 그룹의 합이라
  절대 차이를 Judger만의 변화로 해석하면 안 됩니다.
- 30워커 MBM total +14.61%는 단일 실행이라 원인을 확정할 수 없습니다.
- 다음 단계: 캐시 의존적 워크로드(`chase6` 등)와 혼합 부하에서 반복하고,
  cycles·instructions·LLC miss를 함께 측정합니다.

## 재현

`root` 권한이 필요합니다. stock Judger가 호스트 cgroup 루트에 sandbox를 만들고,
CAT 설정이 resctrl에 쓰기 때문입니다.

1. `cmd/judger-bench`를 Linux amd64로 빌드합니다.
2. `codedang8`의 `/tmp/iris-cat-20261101/`에 다음을 둡니다.
   - `judger-bench-linux-amd64`
   - `scripts/{cat-groups,cat-sample,direct-cat-worker,run-direct-cat-ab}.sh`
     (`scripts/` 하위 디렉터리로)
   - `libjudger.so`(SHA-256 `2c9a4da8…`), `input.txt`, `output.txt`
3. 워크로드는 RDT 실험과 같은 `/tmp/iris-rdt-20261031/workload`
   (SHA-256 `71100d72…`)를 씁니다.
4. 대화형 sudo 세션에서 실행합니다.

```bash
sudo mount -t resctrl resctrl /sys/fs/resctrl   # 이미 마운트되어 있지 않을 때
sudo bash /tmp/iris-cat-20261101/scripts/run-direct-cat-ab.sh \
  8:base:r1 8:equal:r1 16:base:r1 16:equal:r1 30:base:r1 30:equal:r1
```

5. 결과 파일을 로컬 한 디렉터리로 복사해 요약하고 도표를 만듭니다.

```bash
python3 scripts/analyze-direct-cat.py runs/cat-direct-20261101 \
  iris-cat-20261101-r1-base-8 iris-cat-20261101-r1-equal-8 \
  iris-cat-20261101-r1-base-16 iris-cat-20261101-r1-equal-16 \
  iris-cat-20261101-r1-base-30 iris-cat-20261101-r1-equal-30 \
  > runs/cat-direct-20261101/summary.json
python3 scripts/plot-direct-cat.py runs/cat-direct-20261101/summary.json runs/cat-direct-20261101/charts
```

주의:

- 스크립트는 `codedang8`의 CPU 번호와 소켓 배치, 캐시 마스크, `user-1001`
  cgroup 경로를 전제로 검사합니다. 다른 호스트에서는 실패하도록 되어 있습니다.
- 실행 전 다른 resctrl 그룹이 없어야 합니다. 종료 시 CAT 그룹과 sandbox
  cgroup을 지웁니다. 끝난 뒤 `/sys/fs/resctrl`에 `iris-cat-*` 그룹이 남지
  않았는지 확인하고, 직접 마운트했다면 `sudo umount /sys/fs/resctrl`로 되돌립니다.
- run ID의 날짜는 `20261101`, 결과 디렉터리는 `20261031`입니다. 같은 실험입니다.
- `plot-direct-cat.py`는 SVG를 만듭니다. 커밋된 도표는 PNG로 변환한 것입니다.

## 산출물

- 커밋된 요약: `results/cat-direct-20261031/summary.json`, `report.md`, PNG 4장
- 원본: `codedang8:/tmp/iris-cat-20261101/`의 워커별 NDJSON, CAT 마스크 증거,
  로그, RDT TSV
