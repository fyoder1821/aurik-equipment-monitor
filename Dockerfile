FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/aurik-server ./cmd/server

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/aurik-server /aurik-server
EXPOSE 8080
ENTRYPOINT ["/aurik-server"]