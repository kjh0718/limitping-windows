# Windows 지원 마무리 + upstream 동기화 설계

작성: 2026-11-25 · planner(Claude) + Codex(gpt-6-astra) 초안 통합, Codex 리뷰 반영 (approve with changes)

## 의도
issue #1(ConPTY ping/watch/schedule)은 완료. 남은 Windows 공백을 메우고, upstream(wavever/CCLimitPing)과 다시 맞춘 뒤 Windows 작업을 upstream PR로 올린다(전략 A). fork 독자 배포(B)는 하지 않는다.

## 확인된 사실
- upstream Codex ping은 `codex exec --ephemeral --json` headless → 우리 codex.go ConPTY 변경은 불필요. app-server는 reset credit/hooks 전용.
- upstream Claude ping은 `cmd.Dir`/`cmd.Env`로 격리 → 현재 `terminal.Start(ctx, name, args)`로는 전달 불가.
- 양쪽 수정 충돌 파일: README 2개, claude.go, codex.go.
- 이미 있음: ANSI rolling buffer(proxy.go), Codex sessions JSONL fallback, ConPTY close drain.
- Windows 테스트 격리 버그: auth_test가 `HOME`만 바꿔 실제 `~/.claude/.credentials.json`을 읽음(upstream에도 존재) → Phase 0에서 수정.

## 접근 (검토한 대안)
- merge(채택): PR #2가 이미 push된 공유 이력이라 rebase 불가에 가깝고 충돌이 작다.
- rebase: 이력 재작성, 이득 없음.
- 최종 배포: A upstream PR(채택) / B fork 배포(URL·install.ps1 유지 비용).

## Phases
0. **Windows CI** — ci.yml windows-latest job(build/vet/`go test -count=1 -v ./...`), auth_test `USERPROFILE` 격리. 완료: CI 로그에 ConPTY 테스트 실행.
1. **upstream merge + Session API** — codex.go upstream 채택, claude.go에 seam 재적용. `terminal.Start(ctx, *exec.Cmd)`: 지원 필드 Path/Args/Dir/Env 명시, 그 외(Stdin/Stdout/ExtraFiles 등) 설정 시 오류. Windows Env는 UTF-16 block(대소문자 중복 키·nil vs 빈 환경 테스트). headless `codex exec`에도 기존 `.cmd` batch 명령 구성 적용(공백 경로·따옴표·`%` 검증).
2. **upstream 신규 코드 보정** — codex_appserver SIGTERM → 플랫폼 헬퍼(Windows Kill, 정상 완료·취소 모두 검증, `.cmd` 시 트리 정리). live.go `term.GetSize(int(f.Fd()))` (실제 출력 대상, width/height 순서, 실패 시 0).
3. **Windows `continue`** — Session에 Resize/Pid(PID는 `pi.ProcessId` 저장). proxy_windows.go 구현, proxy_unix.go 불변. stdin `term.MakeRaw`(VT input은 x/term이 켬), stdout VT processing on/복원, 250ms polling → ResizePseudoConsole(실패 크기 전달 금지). `.cmd` PID 부정확 시 중복 주입 방지 보수 처리.
4. **upstream PR** — upstream/main 기반 새 브랜치, PR 2개(seam+ConPTY+CI / continue), 영어+zh-CN 문서.

각 phase 검증: `GOOS=windows|linux|darwin go build/vet ./...`, `go test ./...`, LF 기준 gofmt, 해당 수동 smoke.

## SKIP (YAGNI)
winget/scoop/install.ps1, toast 알림, statusline fallback, 적응형 polling, schtasks 자동시작, `%APPDATA%` 이전, Windows self-upgrade, conpty.dll 동봉, ANSI buffer 재구현, app-server job object(잔존 관찰 시).
