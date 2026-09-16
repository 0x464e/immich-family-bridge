FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/familybridge ./cmd/familybridge

FROM alpine:3.22
RUN addgroup -g 1000 familybridge && adduser -D -H -u 1000 -G familybridge familybridge
COPY --from=build /out/familybridge /usr/local/bin/familybridge
USER familybridge
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/familybridge"]
