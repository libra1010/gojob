# gojob dashboard（Vue3 + Vite，构建产物由 nginx 托管并反代 API）
FROM node:24-alpine AS build
WORKDIR /src
COPY dashboard/package.json dashboard/pnpm-lock.yaml ./
RUN corepack enable && pnpm install --frozen-lockfile
COPY dashboard/ .
RUN pnpm build

FROM nginx:1.27-alpine
COPY deploy/nginx-dashboard.conf.template /etc/nginx/templates/default.conf.template
COPY --from=build /src/dist /usr/share/nginx/html
EXPOSE 80
# 模板变量：GOJOB_UPSTREAM（默认 http://gojob-server:7800）
