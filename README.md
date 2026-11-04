# 生物题册 · Biofolio

个人收题、按人教版教材章节或知识点找题、标记易错题和好题，并组卷导出学生版与解析版 Word。支持 DOCX 导入、跨卷重复题核对、本地备份以及可选的 AI 知识点识别与答案草稿审核。公开发行包使用空题库。

## 在 NAS 上运行

从 GitHub Releases 下载 Linux amd64 压缩包，解压到独立项目目录。在项目环境变量中设置 BIOLOGY_LIBRARY_PASSWORD，给服务运行账户授予 data 目录读写权限，然后在绿联 Docker 中用 compose.yaml 创建项目。项目名和容器名均为 `biofolio`，Compose 服务名为 `library`。默认 UID 为 1000，GID 为 10，可用 LIBRARY_UID 和 LIBRARY_GID 调整。通过 NAS 的 18765 端口打开，首次登录用户名为 teacher。可在「设置 → 账号管理」中修改用户名或密码，保存后各设备需要重新登录。修改后的账号保存在服务的数据目录中。远程访问使用私有网络或 HTTPS 入口。

使用内网 AI 网关时，可将 BIOLOGY_AI_ENDPOINT 设为完整的 chat/completions 地址，重建容器后在「AI 设置」中填写对应地址并测试。匹配的请求由题库转发到该固定地址。

从「设置 → 题库备份与迁移」下载 JSON 备份。每批修改后，data/离线题库.html 会更新。空服务器可从匹配初始化样卷的备份恢复。请私下保管实际原卷、备份和模板。

## 多教师共用题库

原账号是管理员。在「设置 → 账号管理」添加教师账号后，每位教师可用自己的用户名、密码登录。题库列表与详情显示收录时的账号名，历史题目显示「上传者 · 未记录」。

题库和试卷共享，每位教师可以用「收藏」标记题目，用「我的收藏」筛选自己的题目。原账号继承旧版好题标记。备份所有账号和个人收藏时需同时保管完整数据目录及账号文件；网页导出的题库备份只含当前账号的收藏。

## 从源码运行

需要 Go（见 go.mod）和 Python 3。前端运行依赖已放在 third_party，构建页面时不需访问 CDN。

```sh
python3 scripts/build-page.py
go test ./backend
go run ./backend --library-page library.html --data ./data
```

本机入口为 http://127.0.0.1:18765 。NAS 压缩包中的 build/biology-library 是同一份 Go 程序的 Linux amd64 构建，build/Dockerfile 对应仓库根目录的 Dockerfile。
