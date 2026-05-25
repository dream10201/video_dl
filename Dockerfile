FROM golang:1.24-bookworm AS build

WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/video_dl .

FROM python:3.12-slim-bookworm

RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
    && pip install --no-cache-dir yt-dlp \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=build /out/video_dl /usr/local/bin/video_dl
ENV PORT=8080 DOWNLOAD_DIR=/app/downloads
VOLUME ["/app/downloads"]
EXPOSE 8080
ENTRYPOINT ["video_dl"]
