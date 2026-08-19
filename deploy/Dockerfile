FROM golang:1.26-alpine AS build
WORKDIR /src
COPY proto ./proto
COPY server ./server
RUN cd server && CGO_ENABLED=0 go build -trimpath -o /out/xnc-server ./cmd/xnc-server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/xnc-server /xnc-server
ENTRYPOINT ["/xnc-server"]
