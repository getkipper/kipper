FROM python:3.12-alpine
WORKDIR /app
COPY server.py entrypoint.sh ./
RUN chmod +x /app/entrypoint.sh && mkdir -p /app/function
EXPOSE 8080
ENTRYPOINT ["/app/entrypoint.sh"]
