# local-native

本机以源码二进制方式跑 Sub2API（非 Docker）。

## 布局

| 路径 | 说明 | Git |
|------|------|-----|
| `scripts/update-and-restart.sh` | 提交 → 拉上游 → 构建（embed）→ 重启 | 跟踪 |
| `runtime/sub2api.env.example` | 环境变量模板 | 跟踪 |
| `runtime/sub2api.env` | 真实密钥 | **忽略** |
| `runtime/data/` | 数据与运行日志 | **忽略** |
| `build/` | 编译产物 | **忽略** |
| `logs/` | 进程 stdout 日志 | **忽略** |

## 首次

```bash
cp runtime/sub2api.env.example runtime/sub2api.env
# 编辑 sub2api.env
./scripts/update-and-restart.sh
```

## 更新

```bash
./scripts/update-and-restart.sh
```

脚本会：

1. **`git add -A` + commit**：提交**整个仓库**当前可跟踪变更（不是只提交 local-native；密钥/build 仍由 gitignore 排除）
2. `git fetch/merge upstream/main`
3. 构建前端 + 后端（**`-tags embed`**）
4. 重启本机进程并校验 `/` 返回 HTML

必须用 **`-tags embed`** 构建，否则只有 API、`/` 全站 SPA 会 `404 page not found`。
