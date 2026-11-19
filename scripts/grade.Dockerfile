FROM node:22-alpine@sha256:1314fb30ec37592d5845e5a160d522c0e680972aa534549940b8d73bde88481a AS node
FROM docker:29-dind@sha256:4bbc7223f81db790097c6dea42fb93e05b8fead4f0431fbe6ccaba6e8473201c
COPY --from=node /usr/local/bin/node /usr/local/bin/node
RUN apk add --no-cache git libstdc++
RUN mv /usr/local/bin/docker /usr/local/bin/docker-real
COPY scripts/grade-docker.mjs /usr/local/bin/docker
RUN chmod 755 /usr/local/bin/docker
ENV GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=safe.directory GIT_CONFIG_VALUE_0=/bot
WORKDIR /kit
