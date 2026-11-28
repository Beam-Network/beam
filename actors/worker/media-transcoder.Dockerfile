FROM debian:bookworm-slim AS build

ARG FFMPEG_VERSION=n7.1.1
ARG NV_CODEC_HEADERS_VERSION=n12.2.72.0
ARG KVAZAAR_VERSION=v2.3.1

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential pkg-config nasm cmake git ca-certificates \
        libopenh264-dev libopus-dev \
    && rm -rf /var/lib/apt/lists/*

RUN git clone --depth 1 --branch "$NV_CODEC_HEADERS_VERSION" https://github.com/FFmpeg/nv-codec-headers.git /src/nv-codec-headers \
    && make -C /src/nv-codec-headers install PREFIX=/usr/local

RUN git clone --depth 1 --branch "$KVAZAAR_VERSION" https://github.com/ultravideo/kvazaar.git /src/kvazaar \
    && cmake -S /src/kvazaar -B /src/kvazaar/build -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/usr/local \
        -DBUILD_SHARED_LIBS=ON -DBUILD_TESTS=OFF \
    && cmake --build /src/kvazaar/build -j"$(nproc)" \
    && cmake --install /src/kvazaar/build

RUN git clone --depth 1 --branch "$FFMPEG_VERSION" https://github.com/FFmpeg/FFmpeg.git /src/ffmpeg
WORKDIR /src/ffmpeg
RUN PKG_CONFIG_PATH=/usr/local/lib/pkgconfig ./configure \
        --prefix=/usr/local \
        --disable-gpl --disable-nonfree \
        --disable-doc --disable-debug --disable-ffplay --disable-ffprobe \
        --disable-network --disable-xlib --disable-sdl2 --disable-vaapi --disable-vdpau \
        --enable-ffnvcodec --enable-nvenc --enable-nvdec --enable-cuvid \
        --enable-libopenh264 --enable-libopus --enable-libkvazaar \
    && make -j"$(nproc)" \
    && make install \
    && ./ffmpeg -hide_banner -L | head -n 3

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends libopenh264-7 libopus0 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /usr/local/bin/ffmpeg /usr/local/bin/ffmpeg
COPY --from=build /usr/local/lib/ /usr/local/lib/
RUN ldconfig && /usr/local/bin/ffmpeg -hide_banner -encoders | grep -E 'h264_nvenc|libopenh264|libkvazaar|libopus'

ENV NVIDIA_DRIVER_CAPABILITIES=compute,video,utility
USER 65534:65534
ENTRYPOINT ["/usr/local/bin/ffmpeg"]
