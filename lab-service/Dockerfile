FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN GOFLAGS=-mod=mod CGO_ENABLED=0 GOOS=linux go build -trimpath -o /mailers-lab .

FROM alpine:3.20
# rclone для загрузки файлов в S3 (стабильно работает с Ceph, в отличие от aws-sdk-go-v2).
# ДО COPY бинарника (2026-08-29) — раньше стояло после, из-за чего ЛЮБОЕ
# изменение Go-кода инвалидировало кэш этого слоя и заставляло качать rclone
# заново при каждом деплое. Источник — GitHub releases с закреплённой версией,
# не downloads.rclone.org/…/rclone-current-… (2026-08-29, найдено живьём:
# downloads.rclone.org оказался недоступен с этого VDS именно в момент
# деплоя кода, вообще не связанного с rclone/S3, — деплой заблокирован без
# всякой причины; GitHub с того же VDS доступен). Версию обновлять вручную
# при необходимости — воспроизводимость сборки важнее автоматического
# "последняя версия".
RUN apk add --no-cache ca-certificates wget unzip \
    && wget -q -O /tmp/rclone.zip https://github.com/rclone/rclone/releases/download/v1.75.0/rclone-v1.75.0-linux-amd64.zip \
    && unzip -q /tmp/rclone.zip -d /tmp/rclone \
    && mv /tmp/rclone/rclone-*-linux-amd64/rclone /usr/local/bin/rclone \
    && chmod +x /usr/local/bin/rclone \
    && rm -rf /tmp/rclone /tmp/rclone.zip \
    && apk del wget unzip
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /mailers-lab /mailers-lab
EXPOSE 3000
ENTRYPOINT ["/mailers-lab"]