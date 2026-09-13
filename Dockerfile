FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -ldflags "-s -w" -o /soldat-master .

FROM scratch
COPY --from=build /soldat-master /soldat-master
EXPOSE 8080
ENTRYPOINT ["/soldat-master"]
