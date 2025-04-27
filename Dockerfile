# ---------- build stage -------------------------------------------------------
    FROM alpine:3.19 AS build
    RUN apk add --no-cache build-base cmake git libwebsockets-dev libuv-dev
    RUN git clone --depth=1 https://github.com/tsl0922/ttyd /src/ttyd \
     && mkdir /src/ttyd/build && cd /src/ttyd/build \
     && cmake -DCMAKE_BUILD_TYPE=MinSizeRel -DBUILD_SHARED_LIBS=OFF .. \
     && make -j$(nproc)
    
    # ---------- final stage -------------------------------------------------------
    FROM alpine:3.19
    LABEL org.opencontainers.image.source="https://github.com/ai-expedite/local-helper"
    
    # tiny busybox http server + OPA binary
    RUN apk add --no-cache busybox-extras curl
    RUN curl -fsSL -o /usr/local/bin/opa \
         https://openpolicyagent.org/downloads/v0.63.0/opa_linux_amd64_static && \
        chmod +x /usr/local/bin/opa
    
    COPY --from=build /src/ttyd/build/ttyd /usr/local/bin/ttyd
    COPY entrypoint.sh helper-shell.sh /usr/local/bin/
    COPY policy.rego /policy/
    
    EXPOSE 3080 3090
    ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]