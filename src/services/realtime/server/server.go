package server

import (
	"net/http"
	"sync"

	"encoding/json"

	"fmt"

	"strings"

	"strconv"

	"github.com/neutrinoapp/neutrino/src/common/client"
	"github.com/neutrinoapp/neutrino/src/common/config"
	"github.com/neutrinoapp/neutrino/src/common/log"
	"github.com/neutrinoapp/neutrino/src/common/messaging"
	"github.com/neutrinoapp/neutrino/src/common/models"
	"gopkg.in/jcelliott/turnpike.v2"
	"gopkg.in/redis.v3"
)

var (
	redisClient      *redis.Client
	natsClient       *client.NatsClient
	messageProcessor messaging.MessageProcessor
)

func init() {
	redisClient = client.GetNewRedisClient()
	messageProcessor = NewClientMessageProcessor()
	natsClient = client.NewNatsClient(config.Get(config.KEY_QUEUE_ADDR))
}

type wsInterceptor struct {
	m chan turnpike.Message
}

func (i *wsInterceptor) Intercept(session turnpike.Session, msg *turnpike.Message) {
	m := *msg
	i.m <- m
}

func Initialize() (*http.Server, error) {
	_, server, c, err := handlerWebSocketServer()
	if err != nil {
		return nil, err
	}

	handleNatsConnection(c)
	handleRpc(c)

	return server, nil
}

func handleRpc(c *turnpike.Client) {
	getArgs := func(args []interface{}) (messaging.Message, *client.ApiClient, error) {
		var m messaging.Message

		incomingMsg := args[0]
		log.Info("RPC message:", incomingMsg)

		b, err := json.Marshal(incomingMsg)
		if err != nil {
			return m, nil, err
		}

		err = json.Unmarshal(b, &m)
		if err != nil {
			return m, nil, err
		}

		c := client.NewApiClientCached(m.App)
		c.Token = m.Token
		if m.Options.Notify != nil {
			c.NotifyRealTime = *m.Options.Notify
		} else {
			c.NotifyRealTime = false
		}

		return m, c, nil
	}

	dataRead := func(args []interface{}, kwargs map[string]interface{}) *turnpike.CallResult {
		m, c, err := getArgs(args)
		if err != nil {
			log.Error(err)
			return &turnpike.CallResult{Err: turnpike.URI(err.Error())}
		}

		var clientResult interface{}
		if id, ok := m.Payload["_id"].(string); ok {
			clientResult, err = c.GetItem(m.Type, id)
		} else {
			clientResult, err = c.GetItems(m.Type)
		}

		if err != nil {
			log.Error(err)
			return &turnpike.CallResult{Err: turnpike.URI(err.Error())}
		}

		return &turnpike.CallResult{Args: []interface{}{clientResult}}
	}

	dataCreate := func(args []interface{}, kwargs map[string]interface{}) *turnpike.CallResult {
		m, c, err := getArgs(args)
		if err != nil {
			log.Error(err)
			return &turnpike.CallResult{Err: turnpike.URI(err.Error())}
		}

		resp, err := c.CreateItem(m.Type, m.Payload)
		if err != nil {
			log.Error(err)
			return &turnpike.CallResult{Err: turnpike.URI(err.Error())}
		}

		log.Info(resp)
		return &turnpike.CallResult{Args: []interface{}{resp["_id"]}}
	}

	dataRemove := func(args []interface{}, kwargs map[string]interface{}) *turnpike.CallResult {
		m, c, err := getArgs(args)
		if err != nil {
			log.Error(err)
			return &turnpike.CallResult{Err: turnpike.URI(err.Error())}
		}

		id, ok := m.Payload["_id"].(string)
		if !ok {
			return &turnpike.CallResult{Err: turnpike.URI(fmt.Sprintf("Incorrect payload, %v", m.Payload))}
		}

		_, err = c.DeleteItem(m.Type, id)
		if err != nil {
			log.Error(err)
			return &turnpike.CallResult{Err: turnpike.URI(err.Error())}
		}

		return &turnpike.CallResult{Args: []interface{}{id}}
	}

	dataUpdate := func(args []interface{}, kwargs map[string]interface{}) *turnpike.CallResult {
		m, c, err := getArgs(args)
		if err != nil {
			log.Error(err)
			return &turnpike.CallResult{Err: turnpike.URI(err.Error())}
		}

		id, ok := m.Payload["_id"].(string)
		if !ok {
			return &turnpike.CallResult{Err: turnpike.URI(fmt.Sprintf("Incorrect payload, %v", m.Payload))}
		}

		_, err = c.UpdateItem(m.Type, id, m.Payload)
		if err != nil {
			log.Error(err)
			return &turnpike.CallResult{Err: turnpike.URI(err.Error())}
		}

		return &turnpike.CallResult{Args: []interface{}{id}}
	}

	c.BasicRegister("data.read", dataRead)
	c.BasicRegister("data.create", dataCreate)
	c.BasicRegister("data.remove", dataRemove)
	c.BasicRegister("data.update", dataUpdate)
}

func handleNatsConnection(c *turnpike.Client) {
	var mu sync.Mutex
	go func() {
		err := natsClient.Subscribe(config.CONST_REALTIME_JOBS_SUBJ, func(mStr string) {
			log.Info("Processing nats message:", mStr)

			var m messaging.Message
			err := m.FromString(mStr)
			if err != nil {
				log.Error(err)
				return
			}

			mu.Lock()
			publishErr := c.Publish(m.Topic, []interface{}{mStr}, nil)
			mu.Unlock()

			if publishErr != nil {
				log.Error(publishErr)
				return
			}
		})

		if err != nil {
			log.Error(err)
			return
		}
	}()
}

func handlerWebSocketServer() (*turnpike.WebsocketServer, *http.Server, *turnpike.Client, error) {
	interceptor := &wsInterceptor{
		m: make(chan turnpike.Message),
	}

	r := turnpike.Realm{}
	r.Interceptor = interceptor

	realms := map[string]turnpike.Realm{}
	realms[config.CONST_DEFAULT_REALM] = r
	wsServer, err := turnpike.NewWebsocketServer(realms)
	if err != nil {
		return nil, nil, nil, err
	}

	wsServer.Upgrader.CheckOrigin = func(r *http.Request) bool {
		//allow connections from any origin
		return true
	}

	c, err := wsServer.GetLocalClient(config.CONST_DEFAULT_REALM, nil)
	if err != nil {
		log.Error(err)
		return nil, nil, nil, err
	}

	go func() {
		for {
			select {
			case m := <-interceptor.m:
				switch msg := m.(type) {
				case *turnpike.Subscribe:
					opts := models.SubscribeOptions{}
					err := models.Convert(msg.Options, &opts)
					if err != nil {
						log.Error(err)
						continue
					}

					if opts.IsSpecial() {
						//remove the last part from 8139ed1ec39a467b96b0250dcf520749.todos.create.2882717310567
						topic := fmt.Sprintf("%v", msg.Topic)
						topicArguments := strings.Split(topic, ".")
						uniqueTopicId := topicArguments[len(topicArguments)-1]
						clientId := strconv.FormatUint(uint64(msg.Request), 10)

						baseTopic := messaging.BuildTopicArbitrary(topicArguments[:len(topicArguments)-1]...)
						opts.BaseTopic = baseTopic
						opts.Topic = topic
						opts.ClientId = msg.Request
						opts.TopicId = uniqueTopicId

						redisClient.SAdd(baseTopic, clientId)

						redisClient.HSet(clientId, "baseTopic", opts.BaseTopic)
						redisClient.HSet(clientId, "topic", opts.Topic)
						redisClient.HSet(clientId, "clientId", clientId)
						redisClient.HSet(clientId, "topicId", opts.TopicId)
						redisClient.HSet(clientId, "filter", models.String(opts.Filter))
					}

				case *turnpike.Publish:
					if len(msg.Arguments) == 0 {
						continue
					}

					m, ok := msg.Arguments[0].(string)
					if !ok {
						continue
					}

					apiError := messageProcessor.Process(m)
					if apiError != nil {
						log.Error(apiError)
					}

					topic := string(msg.Topic)
					log.Info("Sending out special messages:", topic)
					clientIds := redisClient.SMembers(topic).Val()
					msgRaw := models.JSON{}
					err := msgRaw.FromString([]byte(m))
					if err != nil {
						log.Error(err)
						continue
					}

					if payload, ok := msgRaw["pld"].(map[string]interface{}); ok {
						for _, clientId := range clientIds {
							filterString := redisClient.HGet(clientId, "filter").Val()
							filter := models.JSON{}
							filter.FromString([]byte(filterString))

							passes := true
							for k, v := range filter {
								if payload[k] != v {
									passes = false
									break
								}
							}

							if passes {
								topic := redisClient.HGet(clientId, "topic").Val()
								log.Info("Publishing to special topic: ", topic, m)
								err := c.Publish(topic, []interface{}{msgRaw}, nil)
								if err != nil {
									log.Error(err)
									continue
								}
							}

							log.Info(filter)
						}
					}
				case *turnpike.Goodbye:
					clientId := string(msg.Request)
					redisClient.Del(clientId)
				}
			}
		}
	}()

	server := &http.Server{
		Handler: wsServer,
		Addr:    config.Get(config.KEY_REALTIME_PORT),
	}

	return wsServer, server, c, nil
}
