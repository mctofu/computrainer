// test app to connect the grpc server
package main

import (
	"context"
	"io"
	"log"
	"os"

	"github.com/mctofu/computrainer"
	"google.golang.org/grpc"
)

func main() {
	if len(os.Args) < 2 {
		log.Printf("Specify server host to continue\n")
		return
	}

	host := os.Args[1]

	conn, err := grpc.Dial(host + ":8080", grpc.WithInsecure())
	if err != nil {
		log.Fatalf("Failed to connect: %v\n", err)
	}
	defer conn.Close()

	client := computrainer.NewControllerClient(conn)

	ctx := context.Background()

	stream, err := client.GetData(ctx, &computrainer.DataRequest{})
	if err != nil {
		log.Fatalf("Call error: %v\n", err)
	}

	for {
		data, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatalf("Read error: %v\n", err)
		}
		log.Printf("Read: %v\n", data)
	}
}
