FROM node:20-alpine
WORKDIR /app
COPY package.json package-lock.json ./
# npm ci installs exactly what the lockfile pins, so the image ships the same
# tree the licence gate and third-party notices are generated from.
RUN npm ci --omit=dev --no-audit --no-fund
COPY server.js entrypoint.sh ./
RUN chmod +x /app/entrypoint.sh && mkdir -p /app/function
EXPOSE 8080
ENTRYPOINT ["/app/entrypoint.sh"]
