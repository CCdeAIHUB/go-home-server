FROM node:24-alpine AS web

WORKDIR /src/web-console
COPY web-console/package*.json ./
RUN npm install
COPY web-console ./
RUN npm run build

FROM golang:1.25 AS build

WORKDIR /src
ENV GOPROXY=https://goproxy.cn,direct
COPY shared/go ./shared/go
COPY server ./server
WORKDIR /src/server
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/go-home-server ./cmd/server

FROM scratch
WORKDIR /app
COPY --from=build /out/go-home-server /app/go-home-server
COPY --from=web /src/web-console/dist /app/web-console
ENV GO_HOME_ADDR=:8080
ENV GO_HOME_WEB_DIST=/app/web-console
EXPOSE 8080
ENTRYPOINT ["/app/go-home-server"]
