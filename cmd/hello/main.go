package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/gordonklaus/portaudio"
	embedded "github.com/mattetti/ok-go/google.golang.org/genproto/googleapis/assistant/embedded/v1alpha1"
	"google.golang.org/api/option"
	"google.golang.org/api/transport"
	"google.golang.org/grpc"
)

var (
	// Debug allows the caller to see more debug print messages.
	Debug bool
	// keep the state in memory to advance the conversation.
	conversationState []byte
	canceler          context.CancelFunc
	bgCtx             = context.Background()
)

func main() {
	// connect to the audio drivers
	portaudio.Initialize()
	defer portaudio.Terminate()

	start()
}

func newConn(ctx context.Context) (conn *grpc.ClientConn, err error) {
	// connect to Google for a set duration to avoid running forever
	// and charge the user a lot of money.
	return transport.DialGRPC(ctx,
		option.WithEndpoint("embeddedassistant.googleapis.com:443"),
		option.WithScopes("https://www.googleapis.com/auth/assistant-sdk-prototype"),
	)
}

func askTheUser() {
	fmt.Println("Press enter when ready to speak")
	reader := bufio.NewReader(os.Stdin)
	reader.ReadLine()

	start()
	fmt.Println()
}

func start() {
	var (
		ctx  context.Context
		conn *grpc.ClientConn
		err  error
	)
	micStopCh := make(chan bool)

	stop := func(quit bool) {
		micStopCh <- true
		ctx.Done()
		canceler()
		if quit {
			os.Exit(0)
		}
		askTheUser()
	}

	runDuration := 240 * time.Second
	ctx, canceler = context.WithDeadline(bgCtx, time.Now().Add(runDuration))
	conn, err = newConn(ctx)
	if err != nil {
		log.Println("failed to acquire connection", err)
		return
	}
	defer conn.Close()

	assistant := embedded.NewEmbeddedAssistantClient(conn)
	config := &embedded.ConverseRequest_Config{
		Config: &embedded.ConverseConfig{
			AudioInConfig: &embedded.AudioInConfig{
				Encoding:        embedded.AudioInConfig_LINEAR16,
				SampleRateHertz: 16000,
			},
			AudioOutConfig: &embedded.AudioOutConfig{
				Encoding:         embedded.AudioOutConfig_MP3,
				SampleRateHertz:  16000,
				VolumePercentage: 60,
			},
		},
	}
	if len(conversationState) > 0 {
		log.Println("continuing conversation")
		config.Config.ConverseState = &embedded.ConverseState{ConversationState: conversationState}
	}
	conversation, err := assistant.Converse(ctx)
	if err != nil {
		log.Println("failed to setup the conversation", err)
		stop(false)
		os.Exit(1)
	}

	err = conversation.Send(&embedded.ConverseRequest{
		ConverseRequest: config,
	})
	if err != nil {
		fmt.Println("failed to connect to Google Assistant", err)
		stop(false)
		os.Exit(1)
	}

	// listening in the background
	go func() {
		bufIn := make([]int16, 8196)
		var bufWriter bytes.Buffer
		micstream, err := portaudio.OpenDefaultStream(1, 0, 16000, len(bufIn), bufIn)
		if err != nil {
			log.Println("failed to connect to the set the default stream", err)
			stop(false)
			panic(err)
		}
		defer micstream.Close()

		if err = micstream.Start(); err != nil {
			log.Println("failed to connect to the input stream", err)
			stop(false)
			panic(err)
		}
		for {
			bufWriter.Reset()
			if err := micstream.Read(); err != nil {
				log.Println("failed to connect to read from the default stream", err)
				stop(false)
				panic(err)
			}
			binary.Write(&bufWriter, binary.LittleEndian, bufIn)

			err = conversation.Send(&embedded.ConverseRequest{
				ConverseRequest: &embedded.ConverseRequest_AudioIn{
					AudioIn: bufWriter.Bytes(),
				},
			})

			if err != nil {
				log.Printf("Could not send audio: %v", err)
			}
			select {
			case <-micStopCh:
				log.Println("turning off the mic")
				if err = micstream.Stop(); err != nil {
					log.Println("failed to stop the input")
				}
				return
			default:
			}
			if Debug {
				fmt.Print(".")
			}
		}
	}()

	// audio out
	bufOut := make([]int16, 8192)
	// var bufWriter bytes.Buffer
	streamOut, err := portaudio.OpenDefaultStream(0, 1, 16000, len(bufOut), &bufOut)
	defer streamOut.Close()
	if err = streamOut.Start(); err != nil {
		log.Println("failed to start audio out")
		panic(err)
	}
	defer streamOut.Close()

	fmt.Println("Listening")
	// waiting for google assistant response
	for {
		resp, err := conversation.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatalf("Cannot get a response from the assistant: %v", err)
			continue
		}
		if err := resp.GetError(); err != nil {
			log.Fatalf("Received error from the assistant: %v", err)
			continue
		}
		result := resp.GetResult()
		if result == nil {
			log.Println("nil result")
			continue
		}

		log.Println("result from Google Assistant")
		log.Printf("%#v\n", result)
		if transcript := result.GetSpokenRequestText(); transcript != "" {
			log.Printf("Transcript of what you said: %s\n", transcript)
			if transcript == "quit" || transcript == "exit" {
				log.Println("Got it, see you later!")
				stop(true)
				return
			}
		}
		if msg := result.GetSpokenResponseText(); msg != "" {
			log.Printf("Response from the Assistant %s\n", msg)
		}

		// handle the conversation state so the next connection can resume our dialog
		if result.ConversationState != nil {
			conversationState = result.ConversationState
		}
		if resp.GetEventType() == embedded.ConverseResponse_END_OF_UTTERANCE {
			log.Println("Google said we are done, ciao!")
			micStopCh <- true
			return
		}
		audioOut := resp.GetAudioOut()
		if audioOut != nil {
			log.Println("audio out from the assistant")
			signal := bytes.NewReader(audioOut.AudioData)
			for {
				err = binary.Read(signal, binary.LittleEndian, bufOut)
				if err != nil {
					log.Println("failed to read audio out", err)
					break
				}
				if err = streamOut.Write(); err != nil {
					log.Println("failed to write to audio out", err)
					break
				}
			}
		}
		micMode := result.GetMicrophoneMode()
		switch micMode {
		case embedded.ConverseResult_CLOSE_MICROPHONE:
			log.Println("microphone closed")
			stop(false)
			return
		case embedded.ConverseResult_DIALOG_FOLLOW_ON:
			log.Println("continuing dialog")
		default:
			log.Println("unmanaged microphone mode", micMode)
			stop(false)
			return
		}
	}

}