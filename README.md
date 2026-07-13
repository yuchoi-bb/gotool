# gotool

즐겨찾기(바로가기) 런처. 실행하면 마우스 커서 위치에 우클릭 메뉴 모양의 **3열 팝업**이 뜹니다.

```
🌐 웹 즐겨찾기   |   🚀 앱   |   📁 폴더
```

- **왼쪽 클릭** → 해당 항목 실행(웹은 브라우저, 앱은 실행, 폴더는 탐색기)
- **오른쪽 클릭** → 해당 항목 삭제(확인창)
- 항목은 자동 분류됩니다: `.url`(웹 주소) → 웹 / `.lnk` 대상이 폴더 → 폴더 / 그 외 → 앱

AutoHotkey 등으로 단축키에 연결해 쓰기 좋습니다. 예) ESC 두 번:

```ahk
~Esc::
if (A_PriorHotkey = "~Esc" && A_TimeSincePriorHotkey < 400)
    Run "C:\tools\gotool.exe"
return
```

## 다운로드

[Releases](../../releases/latest) 페이지에서 `gotool.exe`를 내려받으세요.

## 바로가기 추가 방법

바로가기는 `gotool.exe` 옆의 **`shortcuts` 폴더**(첫 실행 시 자동 생성)에 파일로 저장됩니다.

1. **탐색기 우클릭으로 추가(추천)** — 한 번만 `gotool.exe install`을 실행하면
   파일/폴더 우클릭 메뉴에 **"gotool에 추가"**가 생깁니다.
   (Windows 11은 "더 많은 옵션 표시" 안에 표시. 메뉴 하단의 등록 항목으로도 설치 가능)
   - 웹 즐겨찾기: 브라우저 주소창의 자물쇠/주소를 바탕화면에 끌어다 놓아 `.url`을 만든 뒤 우클릭 → "gotool에 추가"
   - 앱/폴더: 해당 exe·폴더·바로가기를 우클릭 → "gotool에 추가"
2. **폴더에 직접 넣기** — `.lnk`/`.url`/파일을 `shortcuts` 폴더에 복사만 하면 됩니다. 섞여 있어도 자동으로 3열에 분류됩니다.
3. **명령행** — `gotool.exe add "C:\경로"`

## 수정 · 삭제

- **삭제**: 메뉴에서 항목을 **우클릭** → 확인 → 삭제
- **수정(이름 변경 등)**: 메뉴 맨 아래 **"⚙ 바로가기 폴더 열기"** → 파일 이름을 바꾸면 메뉴 이름이 바뀝니다
- 우클릭 메뉴 등록 해제: `gotool.exe uninstall`

## 명령어 정리

| 명령 | 동작 |
|---|---|
| `gotool.exe` | 3열 메뉴 표시 (exe 옆 `shortcuts` 폴더) |
| `gotool.exe <폴더>` | 지정 폴더의 내용을 메뉴로 표시 |
| `gotool.exe add <경로>` | 바로가기 추가 (`.lnk`/`.url`은 복사, 그 외는 바로가기 생성) |
| `gotool.exe install` | 탐색기 우클릭 "gotool에 추가" 등록 |
| `gotool.exe uninstall` | 우클릭 메뉴 등록 해제 |

## 빌드

```
go build -trimpath -ldflags "-s -w -H windowsgui" -o gotool.exe .
```

`main`에 푸시하거나 `v*` 태그를 만들면 GitHub Actions가 자동으로 빌드해 릴리스에 `gotool.exe`를 올립니다.
