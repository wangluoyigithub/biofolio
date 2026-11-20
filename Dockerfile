FROM python:3-alpine AS page
WORKDIR /src
COPY scripts/build-page.py ./scripts/build-page.py
COPY frontend ./frontend
COPY third_party ./third_party
RUN python3 scripts/build-page.py --out /library.html

FROM golang:1.27-alpine AS server
RUN apk add --no-cache ca-certificates \
 && mkdir -p /certs/etc/ssl/certs \
 && cp /etc/ssl/certs/ca-certificates.crt /certs/etc/ssl/certs/ca-certificates.crt \
 && chmod 0755 /certs/etc /certs/etc/ssl /certs/etc/ssl/certs \
 && chmod 0444 /certs/etc/ssl/certs/ca-certificates.crt
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY backend ./backend
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /biology-library ./backend

FROM scratch
COPY --from=server /certs/etc /etc
COPY --from=server --chmod=0555 /biology-library /biology-library
COPY --from=page --chmod=0444 /library.html /app/library.html
ENTRYPOINT ["/biology-library"]
CMD ["--library-page","/private/library.html","--data","/data","--listen","0.0.0.0:18765"]
