FROM golang:1.22 AS builder

# for local testing:
# ARG FAS_PROMETHEUS_ADDRESS="http://localhost:9090"
# ARG FAS_PROMETHEUS_METRIC_NAME="honcho_deriver_unprocessed_sessions"
# ARG FAS_PROMETHEUS_QUERY="sum(honcho_deriver_unprocessed_sessions{honcho_app_name='$APP_NAME'})"
# ARG FAS_ORG="groudon-testing" 
# ARG FAS_APP_NAME="hch*"
# ARG FAS_PROCESS_GROUP="deriver"
# ARG FAS_STARTED_MACHINE_COUNT="min(10, honcho_deriver_unprocessed_sessions + 1)"

ARG FAS_VERSION=
ARG FAS_COMMIT=
COPY . .
RUN go build -ldflags "-X 'main.Version=${FAS_VERSION}' -X 'main.Commit=${FAS_COMMIT}' -extldflags '-static'" -tags osusergo,netgo -o /usr/local/bin/fly-autoscaler ./cmd/fly-autoscaler

FROM alpine
COPY --from=builder /usr/local/bin/fly-autoscaler /usr/local/bin/fly-autoscaler
ENTRYPOINT [ "fly-autoscaler" ]
CMD [ "serve" ]
