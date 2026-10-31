# 다음 실험 계획하기

다음 실험을 정할 때 쓰는 문서입니다. 앞부분은 지금까지 제안된 후보, 뒷부분은
실험 전에 사용자에게 무엇을 물어야 하는지 정리한 질문 목록입니다. 에이전트는
이 질문에 답을 받기 전에는 호스트를 바꾸거나 긴 실험을 시작하지 않습니다.

## 지금 상태 (2026-10-31 기준)

- 경향은 확립되었습니다. 계산 위주 fixture는 1→30워커에서 약 380→565ms로
  늘고, 재부팅 간 재현성은 높습니다(1·2차).
- 원인은 4차에서 분해했습니다. 계산 위주는 터보·SMT, 메모리 위주는 L3·대역폭·
  NUMA입니다.
- CAT 균등 할당은 계산 위주 fixture에서 효과가 없었습니다. 캐시 의존
  워크로드에서는 아직 레포 도구로 확인하지 않았습니다.
- 모든 결과가 `comparable=false`입니다. alpha.4 Judger가 호스트 루트 cgroup을
  쓰기 때문입니다. 격리(`isolated`) 모드 측정은 아직 한 번도 하지 않았습니다.
- 4차 측정 코드(`meas.c`, `wl.c`, 하드웨어 카운터)는 레포 밖에 있습니다.

## 후보 실험

출처는 각 실험 문서의 "다음 단계"입니다. 우선순위는 제안이며 사용자가 정합니다.

| # | 후보 | 왜 필요한가 | 필요한 것 | 출처 |
| --- | --- | --- | --- | --- |
| 1 | 캐시 의존 워크로드로 CAT A/B 반복 | 계산 위주 fixture로는 CAT 효과를 판단할 수 없음 | `chase6`·혼합 부하 워크로드, 반복 3회 이상, cycles·LLC miss | [CAT](experiments/2026-10-31-cat-equal-ab.md) |
| 2 | 4차 측정기와 메모리 워크로드를 레포로 이관 | 4차를 레포만으로 재현할 수 없음 | `iris-variance-exp/` 코드 정리, `judger-bench`에 perf 카운터 기록 | [4차](experiments/2026-10-29-variance-root-cause.md) |
| 3 | 운영 권고 효과 검증(터보 정책 통일, NUMA 로컬, 물리 코어당 1개) | 권고가 실제 Iris 경로에서 분산을 줄이는지 확인 | server8에서 정책별 사다리, 이후 stage 전후 비교 | [4차](experiments/2026-10-29-variance-root-cause.md) |
| 4 | 수정 libjudger(위임 cgroup)로 격리 모드 측정 | 처음으로 `comparable=true` 모집단 확보, CPU 고정 정책의 전제 | `sandbox-hardening` 빌드, `--qualification-report`, 호스트 자격 통과 | [4차](experiments/2026-10-29-variance-root-cause.md), [WAVE1-REPLICATION §9](WAVE1-REPLICATION.md) |
| 5 | RDT 사다리 반복 | 단계당 1회라 회차 간 변동을 모름 | `scripts/run-rdt-judger-ladder.sh` 일반화 | [RDT](experiments/2026-10-31-rdt-monitoring.md) |
| 6 | open-loop 도착률 실험 | closed loop는 지속 처리량과 coordinated omission 없는 지연을 못 봄 | 도착률 기반 publisher 구현 | [2차](experiments/2026-09-27-full-iris.md) |
| 7 | Iris 결과에 replica 식별자 추가 | 결과를 컨테이너에 귀속할 수 없음 | Iris 쪽 변경 또는 AMQP 메타데이터 | [2차](experiments/2026-09-27-full-iris.md) |
| 8 | client-api HTTP 경로 포함 | 실제 제출 경로 전체 측정 | 쓰기 가능한 DB·Redis·인증 fixture | [2차](experiments/2026-09-27-full-iris.md) |
| 9 | CI 빌드 Go 1.27.3 이미지로 stage 전체 사다리·재부팅 반복 | 3차는 4 replica 2회뿐 | stage 접근, CI 이미지 digest | [3차](experiments/2026-10-21-go-toolchain-ab.md) |

## 사용자에게 물어볼 것

실험을 시작하기 전에 아래 질문의 답을 받습니다. 이미 답이 분명하면 묻지 말고
확인만 합니다. 한 번에 3~5개씩, 선택지를 붙여 묻는 것이 좋습니다.

### 1. 목적

- 이번 실험으로 답하려는 질문은 무엇인가요? 후보 표의 번호를 고르거나 새
  질문을 적어 주세요.
- 결과로 무엇을 결정하나요? 예: 운영 정책 채택, 도구 기능 추가, 회귀 없음 확인.
- 성공 또는 실패를 판단할 기준이 있나요? 예: "CV가 5% 아래로", "중앙값 차이가
  회차 간 변동보다 큼".

### 2. 경로와 워크로드

- 어떤 경로로 측정할까요?
  - 직접 Judger 슈트(가장 단순, 서비스 계층 없음)
  - Full Iris 외부 데이터 슈트(운영 Iris 이미지, AWS 필요)
  - 호스트 직접 실행(CAT·RDT처럼 root 필요)
- 어떤 워크로드를 쓸까요? `cpp-runtime-v1`(계산 위주), 568/15850, 메모리
  워크로드(`chase6`, `stream44`, `chase44`), 또는 혼합.
- 어떤 이미지와 Judger를 쓸까요? 과거 digest 고정인가요, 새로 확인한 값인가요?
  stock alpha.4인가요, 수정 libjudger인가요?

### 3. 규모와 반복

- 동시성 단계는? 기본은 1·2·4·8·16·30입니다.
- 워커당 반복 수는? 과거 값은 50회(1워커는 150회)입니다.
- 회차 반복과 재부팅 반복이 필요한가요? 몇 회인가요?
- A/B라면 비교 대상과 순서는? 번갈아 실행을 권장합니다.

### 4. 호스트와 시간

- `codedang8`을 지금 써도 되나요? 다른 사람이 쓰고 있지 않나요?
- 예상 소요 시간을 알려 주고 괜찮은지 확인합니다. 2차의 단계별 실행 시간은
  55~105초였고, 여기에 load 대기, 수집, 재부팅 시간이 더해집니다.

### 5. 승인이 필요한 변경

다음은 반드시 명시적으로 승인을 받습니다.

- 재부팅
- 터보·최대 클럭·governor 변경
- resctrl 마운트, CAT 그룹 생성
- 위임 cgroup의 `cpuset` 활성화
- `sudo`가 필요한 실행(대화형 비밀번호가 필요할 수 있음)
- Ansible 프로비저닝
- AWS 자격 증명 사용, 새 AWS 리소스나 Terraform 변경
- Notion 페이지 생성이나 수정

### 6. 기록

- 결과를 어디에 남길까요? 이 레포의 `docs/experiments/`, `results/`, Notion
  [Load-test 기록들](https://app.notion.com/p/3dfef9cff54580e099afe69a7a49b99a).
- 원본 번들(`runs/`)은 어디에 보관할까요? Git에는 넣지 않습니다.

## 질문 예시

에이전트가 사용자에게 묻는 예시입니다.

> 다음 실험을 정하기 위해 몇 가지 확인하겠습니다.
>
> 1. 목표: (a) 캐시 의존 워크로드로 CAT A/B 반복 (b) 4차 측정기 레포 이관
>    (c) 수정 libjudger로 격리 모드 측정 (d) 다른 질문
> 2. 경로: (a) 직접 Judger 슈트 (b) Full Iris 외부 데이터 슈트 (c) 호스트 직접 실행
> 3. 규모: 1·2·4·8·16·30워커, 워커당 50회, 3회 반복으로 진행할까요?
> 4. 승인: resctrl 마운트와 CAT 그룹 생성이 필요합니다. 실행 후 원복하고
>    확인하겠습니다. 진행해도 될까요?
> 5. 기록: 결과를 레포 문서와 Notion에 남길까요? Notion 쓰기는 따로 승인이
>    필요합니다.

## 실험 계획서 양식

답을 받으면 실행 전에 아래 양식으로 계획을 적고 사용자 확인을 받습니다.
실험이 끝나면 같은 내용을 `docs/experiments/<YYYY-MM-DD>-<slug>.md`로
남기고 [실험 총정리](experiments/README.md) 표에 한 줄을 추가합니다.

```markdown
# <실험 이름> (<날짜>)

## 질문
## 가설과 판단 기준
## 조건
- 호스트, 커널, CPU 정책(turbo/governor/SMT)
- 경로(슈트), 이미지·Judger digest, 워크로드와 SHA-256
- 동시성 단계, 반복 수, 회차 수
## 바꾸는 호스트 설정과 복원 방법
## 실행 명령
## 결과
## 결론
## 재현
## 산출물 (run ID, 번들 위치, 도표)
## 한계와 다음 단계
```
