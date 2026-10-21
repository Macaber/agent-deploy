# BocomWork Portal

`api-server` 内置的工作空间控制台前端。构建产物是**单文件 HTML**，由 Go 的
`//go:embed portal.html` 直接嵌入二进制并在 `/` 提供访问。

## 技术栈

- React 19 + TypeScript + Vite
- Tailwind CSS v4（shadcn/ui Nova 风格令牌，组件位于 `src/components/ui`）
- Lucide 图标（`lucide-react`）
- Motion 动画（`motion/react`）
- `react-resizable-panels` 可拖拽分栏
- `@dnd-kit/*` 可排序列表（指针 + 键盘）
- Zustand 状态管理（`src/store.ts`）

## 开发

```bash
cd web/portal
npm install
npm run dev          # http://localhost:5173 ，/api 代理到 localhost:3000
```

需要本地运行 `api-server`（默认监听 `:3000`），并提供可用的 kubeconfig。

Node.js 要求：20.19+（Vite 7）。

## 构建

```bash
make portal          # 等价于 cd web/portal && npm ci && npm run build
```

构建会通过 `vite-plugin-singlefile` 把 JS/CSS 全部内联，输出：

1. `web/portal/dist/index.html`
2. `cmd/api-server/portal.html`（构建脚本自动复制，**需要提交到仓库**）

`go build` / `docker build -f Dockerfile.api-server` 直接读取已提交的
`cmd/api-server/portal.html`，因此 CI 不需要安装 Node.js。修改前端后请先执行
`make portal` 再构建镜像。

## 目录

```
src/
  App.tsx                 应用壳、视图切换
  store.ts                Zustand store（目录、表单、目标状态、鉴权、通知）
  lib/api.ts              API 客户端与子路径解析
  lib/workspace.ts        Payload、校验、YAML / cURL 生成
  components/
    header.tsx            顶部导航 / 主题 / 运行中入口
    directory/            工作空间列表（桌面表格 + 移动端卡片）
    studio/               编排表单、dnd-kit 列表、预览与部署面板
    ui/                   Nova 风格基础组件（button/card/dialog/tabs…）
```

## 路由

- `/` 工作空间列表
- `/?tab=studio` 直接打开编排设计器（便于分享和排障）
- API 调用会基于当前页面路径拼接（`getApiUrl()`），适配 Ingress 子路径重写。
