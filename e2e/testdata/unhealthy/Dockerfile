# This fixture always fails its health check to exercise deployment recovery.
FROM busybox:1.37
ARG VERSION
LABEL box-e2e=$VERSION
HEALTHCHECK --interval=1s --timeout=1s --retries=1 CMD false
CMD ["httpd", "-f", "-p", "80"]
