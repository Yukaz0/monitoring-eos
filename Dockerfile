# monitoring-api: binary Go tunggal berisi REST API + scheduler + siklus
# collector (prometheus + portal chromedp). Karena portal butuh Chromium
# headful, image ini menyertakan chromium + xvfb dan menjalankannya via
# xvfb-run. Tidak perlu container collector terpisah.
FROM golang:1.27-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -o /out/monitoring-api ./cmd/api

FROM debian:bookworm-slim
# Chromium (headful via Xvfb) untuk scraper portal, xvfb-run sebagai wrapper.
RUN apt-get update && apt-get install -y --no-install-recommends \
        chromium xvfb xauth ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/monitoring-api /usr/local/bin/monitoring-api
# Healthcheck kecil (subcommand self-check) tanpa wget/curl di image.
RUN apt-get update && apt-get install -y --no-install-recommends netcat-openbsd \
    && rm -rf /var/lib/apt/lists/* || true
COPY docker-entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

# Chromium yang dipakai chromedp (nama binary di Debian).
ENV CHROME_EXEC=/usr/bin/chromium
# Database SQLite di volume.
ENV SQLITE_DSN=file:/data/monitoring.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)

VOLUME ["/data"]
EXPOSE 5118

# Entrypoint sendiri: start Xvfb headful lalu exec API (lihat docker-entrypoint.sh).
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
