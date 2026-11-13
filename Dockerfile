# Scan Box, for amd64; arm64 is not built until it has scanned on a real board. Only the
# Canon (pixma) scanner driver is built into it, from SANE's own release, so nothing else
# from SANE is carried along. SANE's test backend is built beside it for the build's own
# tests of the driver code, and stays out of the image. Build it with deploy/build.sh,
# which tags it with its build ID and image ID.

# The bases are pinned by digest, so the same source builds on the same images. Look up a
# newer one with: docker buildx imagetools inspect golang:1.26-trixie
ARG BUILD_BASE=debian:trixie@sha256:9c6e381421d0f48d1b8133e573f33c185ee16cbf505eda9c4a8a4791b25ea8a6
ARG GO_BASE=golang:1.26-trixie@sha256:19ce7a1fdc59527aa568805ea100c380900d059d251e914fb86e11fd0ce68952
ARG RUNTIME_BASE=debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2

FROM ${BUILD_BASE} AS sane
ARG SANE_VERSION=1.4.0
ARG SANE_SHA256=f99205c903dfe2fb8990f0c531232c9a00ec9c2c66ac7cb0ce50b4af9f407a72
ARG SANE_URL=https://gitlab.com/-/project/429008/uploads/843c156420e211859e974f78f64c3ea3/sane-backends-1.4.0.tar.gz
RUN apt-get update \
	&& apt-get install -y --no-install-recommends build-essential pkg-config libusb-1.0-0-dev libjpeg-dev python3 curl ca-certificates \
	&& rm -rf /var/lib/apt/lists/*
WORKDIR /build
RUN curl -fsSL -o sane.tar.gz "$SANE_URL" \
	&& echo "$SANE_SHA256  sane.tar.gz" | sha256sum -c - \
	&& tar -xzf sane.tar.gz --strip-components=1
RUN ./configure --prefix=/usr --sysconfdir=/etc --localstatedir=/var BACKENDS="pixma test" \
		--with-usb --disable-locking --disable-nls --without-gphoto2 --without-v4l \
		--without-avahi --without-snmp --without-libcurl --without-poppler-glib \
	&& make -j"$(nproc)" \
	&& make install DESTDIR=/out \
	&& rm -rf /out/usr/share /out/usr/bin /out/usr/sbin /out/usr/lib/*.la /out/usr/lib/sane/*.la /out/usr/lib/pkgconfig

FROM ${GO_BASE} AS build
RUN apt-get update \
	&& apt-get install -y --no-install-recommends libusb-1.0-0 libjpeg62-turbo \
	&& rm -rf /var/lib/apt/lists/*
COPY --from=sane /out/usr /usr
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
# The release this is, from the git tag; scanbox -version prints it. Other builds say dev.
ARG VERSION=dev
# The tests run under the race detector, as on a developer's machine; then the SANE driver
# code runs against SANE's test backend, which stands in for a scanner.
RUN go vet -tags sane ./... \
	&& go test -race ./... \
	&& SCANBOX_SANE_TEST_BACKEND=yes go test -race -tags sane -run SANE ./internal/scanner/ \
	&& CGO_ENABLED=1 go build -tags sane -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /scanbox ./cmd/scanbox

# The image Scan Box runs in starts empty. Here its files are gathered under /rootfs: the
# program; the libraries it and the pixma driver load, as ldd finds them, so a library a
# new Debian adds comes along by itself; SANE's two settings files; the CA certificates;
# and /data. Time zones are built into the program. No shell, package manager or setuid
# program is copied.
FROM ${RUNTIME_BASE} AS runtime
# Scan Box runs as user 10001, which owns /data, so a new volume starts out owned by it.
# It opens the scanner through the group of the scanner's device file, given in compose.yaml.
ARG SCANBOX_UID=10001
RUN apt-get update \
	&& apt-get install -y --no-install-recommends libusb-1.0-0 libjpeg62-turbo ca-certificates \
	&& rm -rf /var/lib/apt/lists/*
COPY --from=sane /out/usr/lib/ /usr/lib/
COPY --from=build /scanbox /usr/local/bin/scanbox
WORKDIR /rootfs
# Each library keeps the name programs ask for, in the folder its file is in; the loader
# looks in /usr/lib/x86_64-linux-gnu and /usr/lib by itself. /lib and /lib64 point into
# /usr as in Debian, so the loader is where the program says: /lib64/ld-linux-x86-64.so.2.
# libgcc_s.so.1 is copied by name: glibc loads it itself when a thread ends, as the pixma
# driver's reader thread does after every scan, so ldd doesn't list it. Without it the
# program stops at the end of each scan.
RUN mkdir -p usr/lib/sane usr/lib64 usr/local/bin etc/sane.d etc/ssl/certs \
	&& install -d -m 1777 tmp \
	&& install -d -o "$SCANBOX_UID" -g "$SCANBOX_UID" -m 0700 data \
	&& ldd /usr/local/bin/scanbox /usr/lib/sane/libsane-pixma.so.1 \
		| awk '$2 == "=>" { print $3 } $1 ~ /^\// && $2 ~ /^\(/ { print $1 }' | sort -u \
		| while read -r library; do \
			folder=".$(dirname "$(readlink -f "$library")")"; \
			mkdir -p "$folder" && cp -L "$library" "$folder/$(basename "$library")"; \
		done \
	&& cp -L /usr/lib/sane/libsane-pixma.so.1 usr/lib/sane/ \
	&& cp -L /usr/lib/x86_64-linux-gnu/libgcc_s.so.1 usr/lib/x86_64-linux-gnu/ \
	&& ln -s usr/lib lib && ln -s usr/lib64 lib64 \
	&& ln -s ../lib/x86_64-linux-gnu/ld-linux-x86-64.so.2 usr/lib64/ld-linux-x86-64.so.2 \
	&& cp /usr/local/bin/scanbox usr/local/bin/ \
	&& cp /etc/ssl/certs/ca-certificates.crt etc/ssl/certs/ \
	&& echo pixma > etc/sane.d/dll.conf
# The Debian packages those files come from are recorded as distroless images record them,
# with the release, so a vulnerability scanner such as Trivy still knows their versions.
RUN mkdir -p var/lib/dpkg/status.d \
	&& cp -L /etc/os-release etc/os-release \
	&& for package in ca-certificates $(find usr/lib -name '*.so*' -type f -printf '/%p\n' | xargs dpkg -S 2>/dev/null | cut -d: -f1 | sort -u); do \
		dpkg-query -s "$package" > "var/lib/dpkg/status.d/$package"; \
	done
COPY --from=sane /out/etc/sane.d/pixma.conf /rootfs/etc/sane.d/pixma.conf

FROM scratch
ARG SCANBOX_UID=10001
LABEL org.opencontainers.image.source="https://github.com/aiulian25/scanbox" \
      org.opencontainers.image.description="Scan from a USB scanner into Paperless-ngx, run from one web page" \
      org.opencontainers.image.licenses="MIT"
COPY --from=runtime /rootfs/ /
USER ${SCANBOX_UID}:${SCANBOX_UID}
VOLUME /data
EXPOSE 28761
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/usr/local/bin/scanbox", "-health"]
ENTRYPOINT ["/usr/local/bin/scanbox"]
