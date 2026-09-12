# 单阶段构建:sing-box hub 面板(纯 Go,UI 模板嵌入仓库,无需前端工具链)
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/panel ./cmd/panel

FROM alpine:3.20
RUN adduser -D -u 10001 panel && apk add --no-cache ca-certificates tzdata wget
COPY --from=build /out/panel /usr/local/bin/panel
USER panel
WORKDIR /data
VOLUME /data
EXPOSE 42501
ENTRYPOINT ["panel"]
CMD ["-listen", ":42501", "-data-dir", "/data"]
