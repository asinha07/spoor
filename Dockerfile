# Local build: docker build -t spoor .
# Behind Docker Hub rate limits: --build-arg GO_IMAGE=mirror.gcr.io/library/golang:1.25
ARG GO_IMAGE=golang:1.25
FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/spoor ./cmd/spoor

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/spoor /usr/local/bin/spoor
EXPOSE 8080
ENTRYPOINT ["spoor"]
CMD ["serve", "--http", ":8080"]
