FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /todo .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /todo /todo
USER 65532:65532
ENTRYPOINT ["/todo"]
