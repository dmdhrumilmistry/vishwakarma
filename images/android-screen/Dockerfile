# Android screen sidecar: mirrors an Android device (redroid in the same pod)
# with scrcpy into a virtual X display (TigerVNC Xvnc, which is the X server
# and the VNC server at once, with UTF-8 clipboard sync), so the
# Vishwakarma console can show and control the screen in the browser.
FROM debian:trixie-slim

ARG SCRCPY_VERSION=4.1
# From https://github.com/Genymobile/scrcpy/releases/download/v4.1/SHA256SUMS.txt
ARG SCRCPY_SHA256=ad56ae8bfeedf41e824945c11dbf55fcb092b3e615b9b486f48a50e30d389635

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl tigervnc-standalone-server xclip tini \
 && curl -fsSL -o /tmp/scrcpy.tgz \
      "https://github.com/Genymobile/scrcpy/releases/download/v${SCRCPY_VERSION}/scrcpy-linux-x86_64-v${SCRCPY_VERSION}.tar.gz" \
 && echo "${SCRCPY_SHA256}  /tmp/scrcpy.tgz" | sha256sum -c - \
 && mkdir -p /opt/scrcpy \
 && tar xzf /tmp/scrcpy.tgz -C /opt/scrcpy --strip-components=1 \
 && rm /tmp/scrcpy.tgz \
 && apt-get purge -y curl && apt-get autoremove -y \
 && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh /entrypoint.sh
RUN chmod 0755 /entrypoint.sh

ENV PATH=/opt/scrcpy:$PATH \
    ADB=/opt/scrcpy/adb \
    SCRCPY_SERVER_PATH=/opt/scrcpy/scrcpy-server \
    HOME=/tmp \
    DISPLAY=:0 \
    SDL_VIDEODRIVER=x11 \
    SDL_RENDER_DRIVER=software

LABEL org.opencontainers.image.title="Vishwakarma Android screen" \
      org.opencontainers.image.source="https://github.com/dmdhrumilmistry/vishwakarma" \
      org.opencontainers.image.licenses="Apache-2.0"

USER 65532:65532
ENTRYPOINT ["tini", "--", "/entrypoint.sh"]
