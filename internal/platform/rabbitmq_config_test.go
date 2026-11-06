package platform

import (
	"reflect"
	"testing"
)

func TestUseRabbitMQAcceptsClusterEndpointList(t *testing.T) {
	server := NewServer(nil, "", "")
	first := "amqp://gostream:secret@rabbit-1:5672/"
	second := "amqps://gostream:secret@rabbit-2:5671/vhost"
	if err := server.UseRabbitMQ(first+","+second, "gostream.test"); err != nil {
		t.Fatalf("configure multiple RabbitMQ endpoints: %v", err)
	}
	if got, want := server.broker.urls, []string{first, second}; !reflect.DeepEqual(got, want) {
		t.Fatalf("RabbitMQ endpoints = %#v, want %#v", got, want)
	}
}

func TestUseRabbitMQRejectsEmptyEndpointInList(t *testing.T) {
	server := NewServer(nil, "", "")
	if err := server.UseRabbitMQ("amqp://gostream:secret@rabbit-1:5672/,,amqp://gostream:secret@rabbit-2:5672/", "gostream.test"); err == nil {
		t.Fatal("expected empty RabbitMQ endpoint to be rejected")
	}
}
