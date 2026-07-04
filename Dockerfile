FROM golang:1.24.5 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /sms-gateway .

FROM alpine:3.22
COPY --from=build /sms-gateway /sms-gateway

COPY configs/config.yaml /configs/config.yaml
COPY migrations /migrations
ENV SMS_GW_POSTGRES_MIGRATIONS_PATH=/migrations

EXPOSE 8080

ENTRYPOINT ["/sms-gateway"]
CMD ["serve"]
