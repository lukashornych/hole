FROM alpine:3.19

EXPOSE 8888

RUN apk add --no-cache tinyproxy netcat-openbsd

RUN mkdir -p /var/log/tinyproxy && chown nobody:nobody /var/log/tinyproxy

CMD ["tinyproxy", "-d"]
