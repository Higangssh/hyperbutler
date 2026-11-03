#!/bin/sh
# Answers every HTTP request with: the build-time greeting, the run-time message, a
# counter kept in redis (the db service, reached by its name), and the file the host
# put in the bind-mounted /shared.
mkdir -p /www/cgi-bin
cat > /www/cgi-bin/index <<'EOF'
#!/bin/sh
n=$(redis-cli -h "$DB_HOST" incr hits)
printf 'Content-Type: text/plain\r\n\r\n'
printf 'greeting=%s\n' "$(cat /greeting)"
printf 'message=%s\n' "$APP_MESSAGE"
printf 'hits=%s\n' "$n"
printf 'shared=%s\n' "$(cat /shared/host.txt 2>/dev/null)"
EOF
chmod +x /www/cgi-bin/index
printf '<meta http-equiv="refresh" content="0;url=/cgi-bin/index">' > /www/index.html
exec httpd -f -p 8080 -h /www
