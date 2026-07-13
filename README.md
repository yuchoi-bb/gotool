# gotool

특정 폴더에 들어있는 즐겨찾기(바로가기)를 **실제 아이콘과 함께 팝업 메뉴**로 보여주는 Windows 런처입니다.

- `gotool.exe`를 실행하면 마우스 커서 위치에 즐겨찾기 메뉴가 뜹니다.
- 항목을 클릭하면 해당 바로가기/파일이 열리고 프로그램은 종료됩니다.
- 하위 폴더는 서브메뉴로 표시됩니다.
- `.lnk`, `.url` 바로가기뿐 아니라 일반 파일도 지원합니다.

## 다운로드

[Releases](../../releases/latest) 페이지에서 `gotool.exe`를 내려받으세요.

## 사용법

보여줄 폴더는 아래 순서로 정해집니다.

1. **명령행 인자**: `gotool.exe "C:\내즐겨찾기"`
   (바로가기를 만들어 대상에 폴더 경로를 붙이면 편합니다)
2. **exe 옆의 폴더**: `gotool.exe`와 같은 위치에 `shortcuts`, `favorites`, `즐겨찾기` 중 하나의 폴더가 있으면 그 폴더를 사용
3. **기본값**: 사용자 즐겨찾기 폴더(`%USERPROFILE%\Favorites`)

작업 표시줄이나 시작 화면에 `gotool.exe`를 고정해 두면 클릭 한 번으로 즐겨찾기 메뉴를 열 수 있습니다.

## 빌드

```
go build -trimpath -ldflags "-s -w -H windowsgui" -o gotool.exe .
```

태그(`v*`)를 푸시하면 GitHub Actions가 자동으로 빌드해서 릴리스에 `gotool.exe`를 올립니다.
