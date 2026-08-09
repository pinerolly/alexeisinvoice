# ---- Build stage ----
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -p=1 -o /out/invoiceapp .

# ---- Runtime stage ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/invoiceapp /usr/local/bin/invoiceapp
# Working directory doubles as the persisted data volume (db and json draft).
# Runtime secrets can be mounted separately (e.g. /run/secrets/invoice.env).
WORKDIR /data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/invoiceapp"]
