# Changing VERSION produces a distinct image ID without changing the test server behavior.
FROM busybox:1.37
ARG VERSION
LABEL box-e2e=$VERSION
CMD ["httpd", "-f", "-p", "80"]
