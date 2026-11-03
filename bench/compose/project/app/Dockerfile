FROM alpine:3.20
RUN apk add --no-cache busybox-extras redis
ARG GREETING
RUN echo "$GREETING" > /greeting
COPY serve.sh /serve.sh
RUN chmod +x /serve.sh
EXPOSE 8080
CMD ["/serve.sh"]
