FROM node:24-alpine AS frontend
WORKDIR /app/frontend
COPY frontend/package*.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM golang:1.26-alpine AS backend
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /app/web/static/spa ./web/static/spa
RUN CGO_ENABLED=0 go build -trimpath -o /server ./cmd/server
RUN CGO_ENABLED=0 go build -trimpath -o /worker ./cmd/worker

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app
COPY --from=backend /server /usr/local/bin/server
COPY --from=backend /worker /usr/local/bin/worker
USER app
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/server"]
