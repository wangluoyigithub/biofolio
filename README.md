# 生物题册 · Biofolio

个人收题、按人教版教材章节或知识点找题、标记易错题和好题，并组卷导出学生版与解析版 Word，以及支持 300 DPI 扫描阅卷的机读试卷 PDF 与批改包（内置无头 LibreOffice 自动封版加盖四角定位块与 15mm 矢量页脚二维码）。支持 DOCX 导入、跨卷重复题核对、本地备份以及可选的 AI 知识点识别与答案草稿审核。公开发行包使用空题库。

## 在 NAS 上运行

从 GitHub Releases 下载 Linux amd64 压缩包，解压到独立项目目录。在项目环境变量中设置 BIOLOGY_LIBRARY_PASSWORD，给服务运行账户授予 data 目录读写权限，然后在绿联 Docker 中用 compose.yaml 创建项目。项目名和容器名均为 `biofolio`，Compose 服务名为 `library`。默认 UID 为 1000，GID 为 10，可用 LIBRARY_UID 和 LIBRARY_GID 调整。通过 NAS 的 18765 端口打开，首次登录用户名为 teacher。可在「设置 → 我的账号」中修改用户名或密码，保存后各设备需要重新登录。修改后的账号保存在服务的数据目录中。远程访问使用私有网络或 HTTPS 入口。

使用内网 AI 网关时，可将 BIOLOGY_AI_ENDPOINT 设为完整的 chat/completions 地址，重建容器后在「AI 设置」中填写对应地址并测试。匹配的请求由题库转发到该固定地址。管理员可在「设置 → 共用 AI」保存一次模型名称和 API 密钥，密钥存放在数据目录的 ai-settings.json（不会发到浏览器，也不进备份），之后所有老师无需填写密钥即可使用 AI；老师自己填写的密钥优先。单次 AI 请求默认最多等 120 秒，可用 BIOLOGY_AI_TIMEOUT（10–600 秒）调整。

从「设置 → 备份与迁移」可由管理员下载整库 JSON 备份或单文件 `离线题库.html`。整库 JSON 备份包含全部题目、原卷、全员个人收藏、试题篮、已组试卷、核题草稿及教师账号归属 ID 映射（不含密码哈希）；恢复到空服务器后，管理员用原用户名重新添加教师账号即可自动恢复该教师的题目归属与个人数据。删除的题目、导入试卷和已组试卷先进入「回收站」，可恢复或由管理员彻底移除。请私下保管实际原卷、备份和模板。

## 多教师共用题库

原账号是管理员，只有管理员能看到「账号管理」页面并执行整库备份、恢复或彻底清空回收站。在「账号管理」添加教师账号后，每位教师可用自己的用户名、密码登录。题库列表与详情显示收录时的账号名，历史题目显示「上传者 · 未记录」。

题库、原卷、Word 模板与全库「易错」标记共享（仅管理员或该题收录老师可修改易错状态）；每位教师独立维护自己的「收藏」、「试题篮」与「已组试卷」，互不干扰。

## 从源码或 Docker 直接运行

需要 Go（见 `go.mod`）和 Python 3；也可直接用根目录 `compose.yaml` 多阶段构建运行（镜像内置空库 `library.html`，也可挂载 `./private/library.html` 覆盖）：

```sh
# 方式一：本地直接运行
python3 scripts/build-page.py
go test ./backend
go run ./backend --library-page library.html --data ./data

# 方式二：Docker Compose 一键构建启动
BIOLOGY_LIBRARY_PASSWORD='your-strong-password' docker compose up -d --build
```

本机入口为 http://127.0.0.1:18765 。NAS 压缩包中的 build/biology-library 是同一份 Go 程序的 Linux amd64 构建，build/Dockerfile 对应仓库根目录的 Dockerfile。

## 权限

管理员可以管理全部内容。其他老师只能修改、删除、恢复自己导入的题目和试卷（包括试卷名称、说明和分类、手动补的答案和来源），对别人的题目可以提「建议修改」；回收站的彻底删除只有管理员能做。归属按账号 ID 记录，改用户名不影响归属；旧数据在服务启动时按当时的用户名一次性补上账号 ID，对不上任何老师账号的记为管理员。登录失败按「用户名 + 来源地址」计数，5 次失败锁定 15 分钟。
