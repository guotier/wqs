/*
Copyright 2009-2016 Weibo, Inc.

All files licensed under the Apache License, Version 2.0 (the "License");
you may not use these files except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package queue

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/weibocom/wqs/config"
	"github.com/weibocom/wqs/engine/kafka"
	"github.com/weibocom/wqs/metrics"
	"github.com/weibocom/wqs/model"

	"github.com/Shopify/sarama"
	log "github.com/cihub/seelog"
	"github.com/juju/errors"
)

type queueImp struct {
	conf          *config.Config
	saramaConf    *sarama.Config
	manager       *kafka.Manager
	extendManager *kafka.ExtendManager
	producer      *kafka.Producer
	monitor       *metrics.Monitor
	consumerMap   map[string]*kafka.Consumer
	mu            sync.Mutex
}

func newQueue(config *config.Config) (*queueImp, error) {

	hostname, err := os.Hostname()
	if err != nil {
		return nil, errors.Trace(err)
	}
	sConf := sarama.NewConfig()
	sConf.Net.KeepAlive = 30 * time.Second
	sConf.Metadata.Retry.Backoff = 100 * time.Millisecond
	sConf.Metadata.Retry.Max = 5
	sConf.Metadata.RefreshFrequency = 3 * time.Minute
	sConf.Producer.RequiredAcks = sarama.WaitForLocal
	//conf.Producer.RequiredAcks = sarama.NoResponse //this one high performance than WaitForLocal
	sConf.Producer.Partitioner = sarama.NewRandomPartitioner
	sConf.Producer.Flush.Frequency = time.Millisecond
	sConf.Producer.Flush.MaxMessages = 200
	sConf.ClientID = fmt.Sprintf("%d..%s", os.Getpid(), hostname)
	sConf.ChannelBufferSize = 1024

	extendManager, err := kafka.NewExtendManager(strings.Split(config.MetaDataZKAddr, ","), config.MetaDataZKRoot)
	if err != nil {
		return nil, errors.Trace(err)
	}
	producer, err := kafka.NewProducer(strings.Split(config.KafkaBrokerAddr, ","), sConf)
	if err != nil {
		return nil, errors.Trace(err)
	}
	manager, err := kafka.NewManager(strings.Split(config.KafkaBrokerAddr, ","), config.KafkaLib, sConf)
	if err != nil {
		return nil, errors.Trace(err)
	}

	qs := &queueImp{
		conf:          config,
		saramaConf:    sConf,
		manager:       manager,
		extendManager: extendManager,
		producer:      producer,
		monitor:       metrics.NewMonitor(config.RedisAddr),
		consumerMap:   make(map[string]*kafka.Consumer),
	}
	return qs, nil
}

//Create a queue by name.
func (q *queueImp) Create(queue string) error {
	// 1. check queue name valid
	if len(queue) == 0 {
		errors.NotValidf("CreateQueue queue:%s", queue)
	}

	// 2. check kafka whether the queue exists
	exist, err := q.manager.ExistTopic(queue, true)
	if err != nil {
		return errors.Trace(err)
	}
	if exist {
		return errors.AlreadyExistsf("CreateQueue queue:%s ", queue)
	}

	// 3. check metadata whether the queue exists
	exist, err = q.extendManager.ExistQueue(queue)
	if err != nil {
		return errors.Trace(err)
	}
	if exist {
		return errors.AlreadyExistsf("CreateQueue queue:%s ", queue)
	}

	if err = q.extendManager.AddQueue(queue); err != nil {
		return errors.Trace(err)
	}
	return q.manager.CreateTopic(queue, q.conf.KafkaReplications,
		q.conf.KafkaPartitions, q.conf.KafkaZKAddr)
}

//Updata queue information by name. Nothing to be update so far.
func (q *queueImp) Update(queue string) error {

	if len(queue) == 0 {
		errors.NotValidf("UpdateQueue queue:%s", queue)
	}
	exist, err := q.manager.ExistTopic(queue, true)
	if err != nil {
		return errors.Trace(err)
	}
	if !exist {
		return errors.NotFoundf("UpdateQueue queue:%s ", queue)
	}
	//TODO
	return nil
}

//Delete queue by name
func (q *queueImp) Delete(queue string) error {
	// 1. check queue name valid
	if len(queue) == 0 {
		errors.NotValidf("DeleteQueue queue:%s", queue)
	}

	// 2. check kafka whether the queue exists
	exist, err := q.manager.ExistTopic(queue, true)
	if err != nil {
		return errors.Trace(err)
	}
	if !exist {
		return errors.NotFoundf("DeleteQueue queue:%s ", queue)
	}

	// 3. check metadata whether the queue exists
	exist, err = q.extendManager.ExistQueue(queue)
	if err != nil {
		return errors.Trace(err)
	}
	if !exist {
		return errors.NotFoundf("DeleteQueue queue:%s ", queue)
	}

	if err = q.extendManager.DelQueue(queue); err != nil {
		return errors.Trace(err)
	}
	return q.manager.DeleteTopic(queue, q.conf.KafkaZKAddr)
}

//Get queue information by queue name and group name
//When queue name is "" to get all queue' information.
func (q *queueImp) Lookup(queue string, group string) ([]*model.QueueInfo, error) {

	queueInfos := make([]*model.QueueInfo, 0)
	groupConfigs := make([]*model.GroupConfig, 0)
	switch {
	case queue == "":
		//Get all queue's information
		queueMap, err := q.extendManager.GetQueueMap()
		if err != nil {
			return queueInfos, errors.Trace(err)
		}
		for queueName, groupNames := range queueMap {
			for _, groupName := range groupNames {
				config, err := q.extendManager.GetGroupConfig(groupName, queueName)
				if err != nil {
					return queueInfos, errors.Trace(err)
				}
				if config != nil {
					groupConfigs = append(groupConfigs, &model.GroupConfig{
						Group: config.Group,
						Write: config.Write,
						Read:  config.Read,
						Url:   config.Url,
						Ips:   config.Ips,
					})
				} else {
					log.Warnf("config is nil queue:%s, group:%s", queueName, groupName)
				}
			}

			ctime, err := q.extendManager.QueueCreateTime(queueName)
			if err != nil {
				return queueInfos, errors.Trace(err)
			}
			queueInfos = append(queueInfos, &model.QueueInfo{
				Queue:  queueName,
				Ctime:  ctime,
				Length: 0,
				Groups: groupConfigs,
			})
		}
	case queue != "" && group == "":
		//Get a queue's all groups information
		queueMap, err := q.extendManager.GetQueueMap()
		if err != nil {
			return queueInfos, errors.Trace(err)
		}
		groupNames, exists := queueMap[queue]
		if !exists {
			break
		}
		for _, gName := range groupNames {
			config, err := q.extendManager.GetGroupConfig(gName, queue)
			if err != nil {
				return queueInfos, errors.Trace(err)
			}
			if config != nil {
				groupConfigs = append(groupConfigs, &model.GroupConfig{
					Group: config.Group,
					Write: config.Write,
					Read:  config.Read,
					Url:   config.Url,
					Ips:   config.Ips,
				})
			} else {
				log.Warnf("config is nil queue:%s, group:%s", queue, gName)
			}
		}

		ctime, err := q.extendManager.QueueCreateTime(queue)
		if err != nil {
			return queueInfos, errors.Trace(err)
		}
		queueInfos = append(queueInfos, &model.QueueInfo{
			Queue:  queue,
			Ctime:  ctime,
			Length: 0,
			Groups: groupConfigs,
		})
	default:
		//Get group's information by queue and group's name
		config, err := q.extendManager.GetGroupConfig(group, queue)
		if err != nil {
			return queueInfos, errors.Trace(err)
		}
		if config != nil {
			groupConfigs = append(groupConfigs, &model.GroupConfig{
				Group: config.Group,
				Write: config.Write,
				Read:  config.Read,
				Url:   config.Url,
				Ips:   config.Ips,
			})
		}

		ctime, err := q.extendManager.QueueCreateTime(queue)
		if err != nil {
			return queueInfos, errors.Trace(err)
		}
		queueInfos = append(queueInfos, &model.QueueInfo{
			Queue:  queue,
			Ctime:  ctime,
			Length: 0,
			Groups: groupConfigs,
		})
	}
	return queueInfos, nil
}

func (q *queueImp) AddGroup(group string, queue string,
	write bool, read bool, url string, ips []string) error {

	if len(group) == 0 || len(queue) == 0 {
		errors.NotValidf("add group:%s @ queue:%s", group, queue)
	}

	exist, err := q.manager.ExistTopic(queue, true)
	if err != nil {
		return errors.Trace(err)
	}
	if !exist {
		return errors.NotFoundf("AddGroup queue:%s ", queue)
	}

	if err = q.extendManager.AddGroupConfig(group, queue, write, read, url, ips); err != nil {
		return errors.Trace(err)
	}
	return nil
}

func (q *queueImp) UpdateGroup(group string, queue string,
	write bool, read bool, url string, ips []string) error {

	if len(group) == 0 || len(queue) == 0 {
		errors.NotValidf("add group:%s @ queue:%s", group, queue)
	}

	exist, err := q.manager.ExistTopic(queue, true)
	if err != nil {
		return errors.Trace(err)
	}
	if !exist {
		return errors.NotFoundf("UpdateGroup queue:%s ", queue)
	}

	if err = q.extendManager.UpdateGroupConfig(group, queue, write, read, url, ips); err != nil {
		return errors.Trace(err)
	}
	return nil
}

func (q *queueImp) DeleteGroup(group string, queue string) error {

	if len(group) == 0 || len(queue) == 0 {
		errors.NotValidf("add group:%s @ queue:%s", group, queue)
	}

	exist, err := q.manager.ExistTopic(queue, true)
	if err != nil {
		return errors.Trace(err)
	}
	if !exist {
		return errors.NotFoundf("DeleteGroup queue:%s ", queue)
	}

	if err = q.extendManager.DeleteGroupConfig(group, queue); err != nil {
		return errors.Trace(err)
	}

	return nil
}

//Get group's information
func (q *queueImp) LookupGroup(group string) ([]*model.GroupInfo, error) {

	groupInfos := make([]*model.GroupInfo, 0)
	groupConfigs := make([]*model.GroupConfig, 0)

	if group == "" {
		//GET all groups' information
		groupMap, err := q.extendManager.GetGroupMap()
		if err != nil {
			return groupInfos, errors.Trace(err)
		}
		for groupName, queueNames := range groupMap {
			for _, queueName := range queueNames {
				config, err := q.extendManager.GetGroupConfig(groupName, queueName)
				if err != nil {
					return groupInfos, errors.Trace(err)
				}
				if config != nil {
					groupConfigs = append(groupConfigs, &model.GroupConfig{
						Queue: config.Queue,
						Write: config.Write,
						Read:  config.Read,
						Url:   config.Url,
						Ips:   config.Ips,
					})
				} else {
					log.Warnf("config is nil group:%s, queue:%s", groupName, queueName)
				}
			}
			groupInfos = append(groupInfos, &model.GroupInfo{
				Group:  groupName,
				Queues: groupConfigs,
			})
		}
	} else {
		//GET one group's information
		groupMap, err := q.extendManager.GetGroupMap()
		if err != nil {
			return groupInfos, errors.Trace(err)
		}
		queueNames, exist := groupMap[group]
		if !exist {
			return groupInfos, nil
		}

		for _, queue := range queueNames {
			config, err := q.extendManager.GetGroupConfig(group, queue)
			if err != nil {
				return groupInfos, errors.Trace(err)
			}
			if config != nil {
				groupConfigs = append(groupConfigs, &model.GroupConfig{
					Queue: config.Queue,
					Write: config.Write,
					Read:  config.Read,
					Url:   config.Url,
					Ips:   config.Ips,
				})
			} else {
				log.Warnf("config is nil group:%s, queue:%s", group, queue)
			}
		}
		groupInfos = append(groupInfos, &model.GroupInfo{
			Group:  group,
			Queues: groupConfigs,
		})
	}
	return groupInfos, nil
}

func (q *queueImp) GetSingleGroup(group string, queue string) (*model.GroupConfig, error) {

	exist, err := q.manager.ExistTopic(queue, true)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if !exist {
		return nil, errors.NotFoundf("GetSingleGroup queue:%s ", queue)
	}

	return q.extendManager.GetGroupConfig(group, queue)
}

func (q *queueImp) SendMsg(queue string, group string, data []byte) error {

	exist, err := q.manager.ExistTopic(queue, false)
	if err != nil {
		return errors.Trace(err)
	}
	if !exist {
		return errors.NotFoundf("SendMsg queue:%s ", queue)
	}
	err = q.producer.Send(queue, data)
	if err != nil {
		return errors.Trace(err)
	}
	q.monitor.StatisticSend(queue, group, 1)
	return nil
}

func (q *queueImp) ReceiveMsg(queue string, group string) ([]byte, error) {

	exist, err := q.manager.ExistTopic(queue, false)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if !exist {
		return nil, errors.NotFoundf("ReceiveMsg queue:%s ", queue)
	}

	id := fmt.Sprintf("%s@%s", queue, group)
	q.mu.Lock()
	consumer, ok := q.consumerMap[id]
	if !ok {
		consumer, err = kafka.NewConsumer(strings.Split(q.conf.KafkaBrokerAddr, ","), queue, group)
		if err != nil {
			q.mu.Unlock()
			return nil, errors.Trace(err)
		}
		q.consumerMap[id] = consumer
	}
	q.mu.Unlock()

	data, err := consumer.Recv()
	if err != nil {
		return nil, errors.Trace(err)
	}
	q.monitor.StatisticReceive(queue, group, 1)
	return data, nil
}

func (q *queueImp) AckMsg(queue string, group string) error {
	return errors.NotImplementedf("ack")
}

func (q *queueImp) GetSendMetrics(queue string, group string,
	start int64, end int64, intervalnum int64) (metrics.MetricsObj, error) {

	exist, err := q.manager.ExistTopic(queue, true)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if !exist {
		return nil, errors.NotFoundf("GetSendMetrics queue:%s ", queue)
	}

	return q.monitor.GetSendMetrics(queue, group, start, end, intervalnum)
}

func (q *queueImp) GetReceiveMetrics(queue string, group string, start int64, end int64, intervalnum int64) (metrics.MetricsObj, error) {

	exist, err := q.manager.ExistTopic(queue, true)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if !exist {
		return nil, errors.NotFoundf("GetReceiveMetrics queue:%s ", queue)
	}

	return q.monitor.GetReceiveMetrics(queue, group, start, end, intervalnum)
}
