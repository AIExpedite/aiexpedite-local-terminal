# ---------- build stage -------------------------------------------------------
    FROM alpine:3.19 AS build

    # Install build dependencies including TLS support for libwebsockets
    RUN apk add --no-cache \
        build-base \
        cmake \
        git \
        libwebsockets-dev \
        libwebsockets-evlib_uv \
        libuv-dev \
        zlib-dev \
        json-c-dev \
        openssl-dev
    
    # Clone and build ttyd
    RUN git clone --depth=1 https://github.com/tsl0922/ttyd /src/ttyd \
     && mkdir /src/ttyd/build && cd /src/ttyd/build \
     && cmake -DCMAKE_BUILD_TYPE=MinSizeRel -DBUILD_SHARED_LIBS=OFF .. \
     && make -j$(nproc)
    
    # ---------- final stage -------------------------------------------------------
    FROM alpine:3.19
    
    # Source reference
    LABEL org.opencontainers.image.source="https://github.com/AIExpedite/aiexpedite-local-terminal"
    
    # tiny busybox http server + OPA binary
    RUN apk add --no-cache busybox-extras curl
    RUN curl -fsSL -o /usr/local/bin/opa \
         https://openpolicyagent.org/downloads/v0.63.0/opa_linux_amd64_static && \
        chmod +x /usr/local/bin/opa
    
    # Copy the built ttyd binary
    COPY --from=build /src/ttyd/build/ttyd /usr/local/bin/ttyd
    
    # Copy helper scripts and policy
    COPY entrypoint.sh helper-shell.sh /usr/local/bin/
    COPY policy.rego /policy/
    
    # Expose ports for terminal and token reauth
    EXPOSE 3080 3090
    
    ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
    