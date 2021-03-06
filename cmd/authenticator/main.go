package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	authenticator "github.com/Guillaume-Boutry/face-authenticator-wrapper"
	"github.com/Guillaume-Boutry/grpc-backend/pkg/face_authenticator"
	"github.com/golang/protobuf/proto"
	"log"
	"math"
	"os"
	"path/filepath"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/kelseyhightower/envconfig"
)

// Alias to dlib type
type FeatureMatrix authenticator.Dlib_matrix_Sl_float_Sc_0_Sc_1_Sg_

type work struct {
	faceRequest     *face_authenticator.FaceRequest
	responseChannel chan FeatureMatrix
}

type getResponse struct {
	embeddings []float32
	err        error
}

type Receiver struct {
	client cloudevents.Client

	// If the K_SINK environment variable is set, then events are sent there,
	// otherwise we simply reply to the inbound request.
	Target string `envconfig:"K_SINK"`
	Threshold float32 `envconfig:"THRESHOLD"`
	// Channel to send work
	jobChannel chan *work
}

func main() {
	client, err := cloudevents.NewDefaultClient()
	if err != nil {
		log.Fatal(err.Error())
	}
	// Initializing worker pool
	jobChannel := make(chan *work)
	for w := 1; w <= 4; w++ {
		go worker(w, jobChannel)
	}

	r := Receiver{client: client, jobChannel: jobChannel}
	if err := envconfig.Process("", &r); err != nil {
		log.Fatal(err.Error())
	}

	if err := client.StartReceiver(context.Background(), r.ReceiveAndReply); err != nil {
		log.Fatal(err)
	}
}

type Message struct {
	Payload []byte `json:"payload"`
}

// Request is the structure of the event we expect to receive.
type Request struct {
	Id         string `json:"id"`
	Embeddings string `json:"embeddings,omitempty"`
}

// Response is the structure of the event we send in response to requests.
type Response struct {
	Id         string `json:"id"`
	Message    string `json:"message,omitempty"`
	Embeddings string `json:"embeddings,omitempty"`
}

// ReceiveAndReply is invoked whenever we receive an event.
func (recv *Receiver) ReceiveAndReply(ctx context.Context, event cloudevents.Event) (*cloudevents.Event, cloudevents.Result) {
	req := Message{}
	if err := event.DataAs(&req); err != nil {
		log.Println(err)
		return nil, cloudevents.NewHTTPResult(400, "failed to convert data: %s", err)
	}

	authenticatRequest := &face_authenticator.AuthenticateRequest{}

	if err := proto.Unmarshal(req.Payload, authenticatRequest); err != nil {
		log.Println(err)
		return nil, cloudevents.NewHTTPResult(500, "failed to deserialize protobuf")
	}

	resChannel := make(chan getResponse)
	defer close(resChannel)
	go func() {
		embeddingsRef, err := recv.getEmbeddings(ctx, authenticatRequest.FaceRequest.Id)
		if err != nil {
			log.Println(err)
		}
		resChannel <- getResponse{
			embeddings: embeddingsRef,
			err:        err,
		}
	}()
	responseChannel := make(chan FeatureMatrix)
	recv.jobChannel <- &work{
		faceRequest:     authenticatRequest.FaceRequest,
		responseChannel: responseChannel,
	}
	embeddingsResponse := <-resChannel
	embeddings := <-responseChannel
	if embeddingsResponse.err != nil {
		log.Println(embeddingsResponse.err)
		return nil, cloudevents.NewHTTPResult(500, "Error while getting reference embeddings")
	}
	ptr := &embeddingsResponse.embeddings[0]
	embeddingsRef := authenticator.Deserialize_embeddings(ptr)

	authent := authenticator.NewAuthenticator(0)
	defer authenticator.DeleteAuthenticator(authent)
	score := float32(authent.ComputeDistance(embeddings, embeddingsRef))
	fmt.Printf("Score %f\n", score)
	decision := score < recv.Threshold
	authenticateResponse := &face_authenticator.AuthenticateResponse{
		Status:   face_authenticator.AuthenticateStatus_AUTHENTICATE_STATUS_OK,
		Message:  fmt.Sprintf("%s authenticated with success", authenticatRequest.FaceRequest.Id),
		Score:    score,
		Decision: decision,
	}
	resp, err := proto.Marshal(authenticateResponse)
	if err != nil {
		log.Println(err)
		return nil, cloudevents.NewHTTPResult(500, "failed to serialize response")
	}
	r := cloudevents.NewEvent(cloudevents.VersionV1)
	r.SetType("authenticate-response")
	r.SetSource("authenticator")
	msg := Message{Payload: resp}
	if err := r.SetData("application/json", msg); err != nil {
		return nil, cloudevents.NewHTTPResult(500, "failed to set response data")
	}

	return &r, nil
}

func (recv *Receiver) getEmbeddings(ctx context.Context, id string) ([]float32, error) {
	r := cloudevents.NewEvent(cloudevents.VersionV1)
	r.SetType("get")
	r.SetSource("authenticator")

	req := &Request{
		Id: id,
	}
	if err := r.SetData("application/json", req); err != nil {
		log.Println(err)
		return nil, err
	}
	newCtx := cloudevents.ContextWithTarget(ctx, recv.Target)
	response, res := recv.client.Request(newCtx, r)
	if cloudevents.IsUndelivered(res) {
		log.Printf("Failed to request: %v", res)
		return nil, res
	} else if response != nil {
		log.Printf("Got Event Response Context: %+v\n", response.Context)
	} else {
		// Parse result
		log.Printf("Event sent at %s", time.Now())
		return nil, errors.New("error get embeddings failed")
	}
	responseObject := &Response{}
	if err := response.DataAs(responseObject); err != nil {
		return nil, errors.New("error parsing response")
	}

	if len(responseObject.Embeddings) == 0 {
		return nil, errors.New("got empty embeddings from database")
	}

	bytes, err := base64.StdEncoding.DecodeString(responseObject.Embeddings)
	if err != nil {
		return nil, err
	}
	embeddings, err := bytesToFloatArray(bytes)
	if err != nil {
		return nil, err
	}

	return embeddings, nil
}

func validRectangle(coordinates *face_authenticator.FaceCoordinates) bool {
	return coordinates.TopLeft != nil && coordinates.TopLeft.X != 0 && coordinates.TopLeft.Y != 0 && coordinates.BottomRight != nil && coordinates.BottomRight.X != 0 && coordinates.BottomRight.Y != 0
}

func worker(idThread int, jobs <-chan *work) {
	authent := authenticator.NewAuthenticator(32)
	defer authenticator.DeleteAuthenticator(authent)
	log.Printf("Thread %d: Init authenticator\n", idThread)
	modelDir, pres := os.LookupEnv("model_dir")
	if !pres {
		modelDir = "/opt/authenticator"
	}
	authent.Init(filepath.Join(modelDir, "shape_predictor_5_face_landmarks.dat"), filepath.Join(modelDir, "dlib_face_recognition_resnet_model_v1.dat"))
	log.Printf("Thread %d: Ready to authenticate\n", idThread)
	for job := range jobs {
		generateEmbeddings(&authent, job, idThread)
	}
}

func generateEmbeddings(authent *authenticator.Authenticator, work *work, idThread int) {
	facereq := work.faceRequest
	cImgData := authenticator.Load_mem_jpeg(&facereq.Face[0], len(facereq.Face))
	defer authenticator.DeleteImage(cImgData)
	var facePosition authenticator.Rectangle
	log.Printf("Thread %d: Searching for a face...\n", idThread)
	if coords := facereq.FaceCoordinates; coords == nil || !validRectangle(coords) {
		facePosition = (*authent).DetectFace(cImgData)
		defer authenticator.DeleteRectangle(facePosition)
	} else {
		facePosition = authenticator.NewRectangle()
		facePosition.SetTop(coords.TopLeft.Y)
		facePosition.SetLeft(coords.TopLeft.X)
		facePosition.SetBottom(coords.BottomRight.Y)
		facePosition.SetRight(coords.BottomRight.X)
	}
	log.Printf("Thread %d: Found face in area top_left(%d, %d), bottom_right(%d, %d)\n", idThread, facePosition.GetTop(), facePosition.GetLeft(), facePosition.GetBottom(), facePosition.GetRight())
	extractedFace := (*authent).ExtractFace(cImgData, facePosition)
	defer authenticator.DeleteImage(extractedFace)
	log.Printf("Thread %d: Generating embeddings\n", idThread)
	embeddings := (*authent).GenerateEmbeddings(extractedFace)
	work.responseChannel <- embeddings
}

func bytesToFloatArray(bytes []byte) ([]float32, error) {
	if len(bytes)%4 != 0 {
		return nil, errors.New("bytes in input aren't a multiple of 4")
	}
	lenArr := len(bytes) / 4
	array := make([]float32, lenArr)
	for i := 0; i < lenArr; i++ {
		array[i] = float32frombytes(bytes[i*4 : (i*4)+4])
	}
	return array, nil
}

func float32frombytes(bytes []byte) float32 {
	bits := binary.LittleEndian.Uint32(bytes)
	float := math.Float32frombits(bits)
	return float
}
