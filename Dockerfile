# Polka — home library server.
# Build: docker build -t polka .
# Run:   docker run -p 12791:12791 -v polka-data:/data -v /path/to/books:/books polka

# --- Frontend ---
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# --- Server (pure Go, no CGO) ---
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
ARG VERSION=docker
# -tags nodynamic: decode JPEG XL covers via the pure-Go wazero backend
# (no purego/CGO), so the build stays CGO-free across all targets.
RUN CGO_ENABLED=0 go build -trimpath -tags nodynamic -ldflags "-s -w -X main.version=${VERSION}" -o /polka ./cmd/polka

# --- FB2 converter ---
# fbc (github.com/rupor-github/fb2cng, GPL-3.0) is a separate program that
# polka runs to offer EPUB / KEPUB / KFX / PDF downloads of FB2 books; it is
# found next to the polka binary. Releases exist for amd64 and arm64 only —
# on any other platform the image is built without it and simply does not
# offer conversion. Bumping FBC_VERSION means updating both checksums.
FROM alpine:3.21 AS fbc
ARG TARGETARCH
ARG FBC_VERSION=v1.8.1
RUN mkdir /out; \
    case "$TARGETARCH" in \
      amd64) sum=27b07665e4a03c02b1696fbfe67ac583b4e6519e56d509bb1ef0c13d8ff36758 ;; \
      arm64) sum=7ab64f1709a8f24a91e98d3191b9ee8019d77b63ccfbac61ef1645b5212486b7 ;; \
      *) echo "no fbc release for '$TARGETARCH': building without the converter"; exit 0 ;; \
    esac; \
    wget -q -O /tmp/fbc.zip "https://github.com/rupor-github/fb2cng/releases/download/${FBC_VERSION}/fbc-linux-${TARGETARCH}.zip" \
    && echo "$sum  /tmp/fbc.zip" | sha256sum -c - \
    && unzip -q /tmp/fbc.zip fbc -d /out \
    && chmod 755 /out/fbc

# --- Runtime ---
FROM alpine:3.21
RUN adduser -D -H polka && mkdir -p /data /books && chown polka /data /books
COPY --from=build /polka /usr/local/bin/polka
COPY --from=fbc /out/ /usr/local/bin/
USER polka
# Defaults via env so any subcommand (serve, import, passwd) run with
# `docker exec` uses the data/library volumes, not the user's home dir.
ENV POLKA_DATA_DIR=/data \
    POLKA_LIBRARY_DIR=/books \
    POLKA_ADDR=:12791
VOLUME ["/data", "/books"]
EXPOSE 12791
ENTRYPOINT ["polka"]
CMD ["serve"]
