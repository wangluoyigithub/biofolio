FROM --platform=$BUILDPLATFORM python:3-alpine AS page
WORKDIR /src
COPY scripts/build-page.py ./scripts/build-page.py
COPY frontend ./frontend
COPY third_party ./third_party
RUN python3 scripts/build-page.py --out /library.html

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS server
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY backend ./backend
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w' -o /biology-library ./backend

FROM alpine:3.21
RUN apk add --no-cache \
      ca-certificates \
      tzdata \
      libreoffice-writer \
      font-noto-cjk \
      ttf-dejavu \
 && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
 && echo "Asia/Shanghai" > /etc/timezone
RUN mkdir -p /app && chmod 0755 /app \
 && rm -rf /tmp && ln -s /data/.lo-tmp /tmp \
 && (grep -q '^biofolio:' /etc/passwd || echo 'biofolio:x:1000:10:biofolio:/data/.lo-tmp:/sbin/nologin' >> /etc/passwd)
COPY --from=server --chmod=0555 /biology-library /biology-library
COPY --from=page --chmod=0444 /library.html /app/library.html
ENTRYPOINT ["/biology-library"]
CMD ["--library-page","/private/library.html","--data","/data","--listen","0.0.0.0:18765"]
