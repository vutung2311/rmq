package rmq

import (
	"time"
)

type TestConsumer struct {
	name          string
	AutoAck       bool
	AutoFinish    bool
	SleepDuration time.Duration

	LastDelivery   Delivery
	LastDeliveries []Delivery

	finish chan int
}

func NewTestConsumer(name string) *TestConsumer {
	return &TestConsumer{
		name:       name,
		AutoAck:    true,
		AutoFinish: true,
		finish:     make(chan int),
	}
}

func (consumer *TestConsumer) String() string {
	return consumer.name
}

func (consumer *TestConsumer) Consume(delivery Delivery) {
	consumer.LastDelivery = delivery
	consumer.LastDeliveries = append(consumer.LastDeliveries, delivery)

	if consumer.SleepDuration > 0 {
		time.Sleep(consumer.SleepDuration)
	}
	if consumer.AutoAck {
		delivery.Ack()
	}
	if !consumer.AutoFinish {
		<-consumer.finish
	}
}

func (consumer *TestConsumer) Finish() {
	consumer.finish <- 1
}

type CustomTestConsumer struct {
	consumeFunc func(delivery Delivery)
}

func NewCustomTestConsumer(deliveryFunc func(Delivery)) *CustomTestConsumer {
	return &CustomTestConsumer{
		consumeFunc: deliveryFunc,
	}
}

func (consumer *CustomTestConsumer) Consume(delivery Delivery) {
	if consumer.consumeFunc != nil {
		consumer.consumeFunc(delivery)
		return
	}
}
