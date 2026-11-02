# 문서 목록

## 처음 읽을 문서

| 문서 | 내용 |
| --- | --- |
| [실험 총정리](experiments/README.md) | 파일럿부터 CAT A/B까지 실험 연대기, 결론, 비교표 |
| [재현 가이드](REPRODUCTION.md) | 공통 준비, 실행 흐름, 실행 중·후 확인 사항 |
| [다음 실험 계획](NEXT-EXPERIMENT.md) | 후보 실험과 사용자에게 물어볼 질문 |

## 실험 기록 (한국어)

| 날짜 | 문서 |
| --- | --- |
| 2026-09-18 | [파일럿: 운영 노드에서 Iris 1→30대](experiments/2026-09-18-pilot.md) |
| 2026-09-26 | [1차: 직접 Judger 사다리와 재부팅 반복](experiments/2026-09-26-direct-judger.md) |
| 2026-09-27 | [2차: Full Iris 외부 데이터 사다리와 재부팅 반복](experiments/2026-09-27-full-iris.md) |
| 2026-10-21 | [3차: Go 1.25.14 → 1.27.3 회귀 확인](experiments/2026-10-21-go-toolchain-ab.md) |
| 2026-10-29~31 | [4차: 분산 원인 규명](experiments/2026-10-29-variance-root-cause.md) |
| 2026-10-31 | [RDT: CMT·MBM 동시 관측](experiments/2026-10-31-rdt-monitoring.md) |
| 2026-10-31 | [CAT: L3 균등 할당 A/B](experiments/2026-10-31-cat-equal-ab.md) |

## 운영 가이드 (영문)

| 문서 | 내용 |
| --- | --- |
| [WAVE1-REPLICATION.md](WAVE1-REPLICATION.md) | 파일럿(Wave 1) 재현 매뉴얼. Track A(Kubernetes)와 Track B(이 도구) |
| [WAVE1-COMPARISON.md](WAVE1-COMPARISON.md) | 파일럿 기준값과 1차 직접 Judger 재현 비교 |
| [FULL-IRIS-EXTERNAL.md](FULL-IRIS-EXTERNAL.md) | Full Iris 외부 데이터 슈트 운영 절차 |
| [RDT-MONITORING.md](RDT-MONITORING.md) | `iris-bench-rdt` 수동 CMT/MBM 측정 |
| [CAT-DIRECT-AB.md](CAT-DIRECT-AB.md) | 호스트 직접 CAT A/B 절차 |

## 레퍼런스

| 문서 | 내용 |
| --- | --- |
| [reference/TOOLSET-REFERENCE.md](reference/TOOLSET-REFERENCE.md) | CLI 플래그, 설정 스키마, 격리, SSH, Ansible, AWS, 검증 (영문) |
| [../AGENTS.md](../AGENTS.md) | 에이전트용 명령, 실행 조건, 안전 규칙 (영문) |
| [../fixtures/README.md](../fixtures/README.md) | fixture 구성 |
| [../scripts/aws/README.md](../scripts/aws/README.md) | AWS 스크립트 |
| [../infra/aws/iris-benchmark/README.md](../infra/aws/iris-benchmark/README.md) | 벤치마크 Terraform 모듈 |
| [../infra/aws/bootstrap/README.md](../infra/aws/bootstrap/README.md) | AWS 배포자 bootstrap |

## 원문

실험 원문과 도표는 Notion
[Load-test 기록들](https://app.notion.com/p/3dfef9cff54580e099afe69a7a49b99a)에
있습니다. 이 레포 문서는 그 내용을 요약하고 코드와 연결합니다.
