package main

import (
	"context"
	"fmt"
	"time"

	pb "github.com/andrew-delph/my-key-store/proto"

	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

func (s *internalServer) TestRequest(ctx context.Context, in *pb.StandardResponse) (*pb.StandardResponse, error) {
	logrus.Warnf("Received: %v", in.Message)
	return &pb.StandardResponse{Message: "This is the server."}, nil
}

func SendSetMessage(key, value string) error {
	setReqMsg := &pb.SetRequestMessage{Key: key, Value: value}

	nodes, err := GetClosestN(events.consistent, key, totalReplicas)
	if err != nil {
		return err
	}

	responseCh := make(chan *pb.StandardResponse, totalReplicas)
	errorCh := make(chan error, totalReplicas)

	for _, node := range nodes {
		go func(currNode HashRingMember) {
			conn, client, err := GetClient(currNode.String())
			defer conn.Close()

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			r, err := client.SetRequest(ctx, setReqMsg)
			if err != nil {
				logrus.Errorf("Failed SetRequest for node %s", currNode.String())
				errorCh <- err
			} else {
				responseCh <- r
				logrus.Warnf("SetRequest %s worked. msg ='%s'", currNode.String(), r.Message)
			}
		}(node)
	}

	responseCount := 0
	for responseCount < writeResponse {
		select {
		case <-responseCh:
			responseCount++
		case err := <-errorCh:
			_ = err
		}
	}

	return nil
}

func SendGetMessage(key string) (string, error) {
	getReqMsg := &pb.GetRequestMessage{Key: key}

	nodes, err := GetClosestN(events.consistent, key, totalReplicas)
	if err != nil {
		return "", err
	}

	getSet := make(map[string]int)
	responseCh := make(chan string, len(nodes))
	errorCh := make(chan error, len(nodes))

	for i, node := range nodes {
		go func(i int, node HashRingMember) {
			conn, client, err := GetClient(node.String())
			if err != nil {
				errorCh <- err
				return
			}
			defer conn.Close()

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			r, err := client.GetRequest(ctx, getReqMsg)
			if err != nil {
				errorCh <- err
			} else {
				logrus.Warnf("GetRequest %s value='%s'", node.String(), r.Value)
				responseCh <- r.Value
			}
		}(i, node)
	}

	for responseCount := 0; responseCount < len(nodes); responseCount++ {
		select {
		case value := <-responseCh:
			getSet[value]++
			if getSet[value] >= readResponse {
				return value, nil
			}
		case <-errorCh:
			// Handle error if necessary
		}
	}

	return "", fmt.Errorf("value not found.")
}

func GetClient(addr string) (*grpc.ClientConn, pb.InternalNodeServiceClient, error) {
	conn, err := grpc.Dial(fmt.Sprintf("%s:%d", addr, port), grpc.WithInsecure(), grpc.WithBlock())
	if err != nil {
		return nil, nil, err
	}
	internalClient := pb.NewInternalNodeServiceClient(conn)

	return conn, internalClient, nil
}
