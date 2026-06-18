FROM node:20-alpine AS frontend-builder
WORKDIR /app
COPY dashboard/package*.json ./
RUN npm ci
COPY dashboard/ ./
RUN npm run build

FROM golang:alpine AS backend-builder
WORKDIR /app
COPY go.mod go.sum ./
COPY . ./
RUN go mod tidy
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -o telemetry-server ./cmd/server

FROM alpine:latest
RUN apk --no-cache add ca-certificates tzdata
WORKDIR /app
# Copy the compiled Go API
COPY --from=backend-builder /app/telemetry-server /app/telemetry-server
# Copy the compiled React website
COPY --from=frontend-builder /app/dist /app/dashboard/dist

ENV PORT=8080
ENV ENV=prod

EXPOSE 8080

CMD ["/app/telemetry-server"]
