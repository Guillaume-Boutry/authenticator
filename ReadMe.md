# Authenticator

This project is responsible to produce authentication results !

It compares the image sent by the grpc backend to the one corresponding in cassandra !

## Build

A docker file is present to build it !

Just run:

```bash
docker build . -t registry.zouzland.com/authenticator:snapshot
```

## Configuration

The different configuration are the following:
```bash
model_dir=/opt/authenticator # Path the default model used by the neural net !
K_SINK=http://data-controller.default # Url to the datacontroller
THRESHOLD=0.25 # Threshold under which two person are judged the same (remember it's a distance between two vectors)
```

Override them with environment variables !