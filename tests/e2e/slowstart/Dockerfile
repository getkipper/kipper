# A web app that takes START_DELAY seconds to open its port, like a JVM that is
# still starting. Without a readiness check its new pod takes traffic before it
# listens, and the route answers 502 for that long.
FROM busybox:1.37
RUN mkdir -p /www && echo slowstart > /www/index.html
ENV START_DELAY=25
EXPOSE 8080
CMD ["sh", "-c", "sleep \"$START_DELAY\" && exec httpd -f -p 8080 -h /www"]
