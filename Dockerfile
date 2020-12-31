FROM registry.zouzland.com/face-authenticator-builder AS builder
WORKDIR /app
COPY go.* ./
RUN go mod download
COPY . .
RUN GOOS=linux go build -v ./cmd/authenticator

FROM registry.zouzland.com/face-authenticator-runner
COPY models.txt models.txt
RUN wget -i models.txt --directory-prefix=/opt/authenticator && bzip2 -d $(ls /opt/authenticator/*.bz2)
COPY --from=builder /app/authenticator /opt/authenticator/authenticator
# Run the web service on container startup.
CMD ["/opt/authenticator/authenticator"]