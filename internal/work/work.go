package work

import (
	"context"
	"github.com/apache/pulsar-client-go/pulsar"
	"github.com/bytedance/sonic"
	"github.com/heyehang/go-im-grpc/user_server"
	"github.com/heyehang/go-im-pkg/pulsarsdk"
	"github.com/panjf2000/ants/v2"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/zrpc"
	"go-im-chat-server/internal/config"
	"go-im-chat-server/internal/dao/model"
	"go-im-chat-server/internal/dao/mongo"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"sync"
	"time"
)

type Work struct {
	pool            *ants.Pool
	con             config.Config
	userCli         user_server.UserClient
	producer        *pulsarsdk.Producer
	insertMongoPool *sync.Pool
}

func NewWork(c config.Config) *Work {
	w := new(Work)
	pool, err := ants.NewPool(c.WorkPoolSize, ants.WithNonblocking(true))
	if err != nil {
		panic(err)
		return nil
	}
	w.con = c
	w.pool = pool
	w.userCli = user_server.NewUserClient(zrpc.MustNewClient(c.UserSrv, zrpc.WithTimeout(time.Second*5), zrpc.WithNonBlock()).Conn())
	prod, err := pulsarsdk.NewProducer(c.Pulsar.WorkTopic, 5)
	if err != nil {
		panic(err)
		return nil
	}
	w.producer = prod
	poollNewFunc := func() any {
		buf := make([]interface{}, 0, 3)
		return buf
	}
	w.insertMongoPool = &sync.Pool{New: poollNewFunc}
	return w
}

func (w *Work) Start(ctx context.Context) {
	pulsarsdk.SubscribeMsg(ctx, w.con.Pulsar.Topic, func(message pulsar.Message, err error) {
		if err != nil {
			logx.Errorf("SubscribeMsg_Unmarshal_err :%+v", err)
			return
		}
		data := message.Payload()
		msg := new(model.Msg)
		err = sonic.Unmarshal(data, msg)
		if err != nil {
			logx.Errorf("SubscribeMsg_Unmarshal_err :%+v", err)
			return
		}
		msg.ID = primitive.NewObjectID()
		msg.MsgId = msg.ID.String()
		msg.Seq = uint64(time.Now().UnixNano())
		insertCtx, insertCtxCancel := context.WithTimeout(context.Background(), time.Second*30)
		defer insertCtxCancel()
		buf, ok := w.insertMongoPool.Get().([]interface{})
		if !ok {
			buf = make([]interface{}, 0, 1)
		}
		buf = append(buf, msg)
		err = mongo.InsertMsg(insertCtx, buf)
		if err != nil {
			logx.Errorf("SubscribeMsg_InsertMsg_err :%+v", err)
			return
		}
		// ????????????
		batch := int64(50)
		skip := int64(0)
		members := make([]*model.ChatMember, 0, 50)
		for {
			queryCtx, cancel := context.WithTimeout(context.Background(), time.Second*10)
			defer cancel()
			uids, err := mongo.GetMembersByChatID(queryCtx, msg.ChatID, batch, skip)
			if err != nil {
				cancel()
				logx.Errorf("SubscribeMsg_GetMembersByChatID_err :%+v", err)
				return
			}
			members = append(members, uids...)
			skip = skip + batch
			if len(uids) < int(batch) {
				break
			}
		}
		if len(members) <= 0 {
			return
		}
		req := new(user_server.GetDeviceTokensByUserIDReq)
		for i := 0; i < len(members); i++ {
			req.Ids = append(req.Ids, members[i].UserID)
		}
		grpcCtx, cancel := context.WithTimeout(context.Background(), time.Second*10)
		defer cancel()
		resp, err := w.userCli.GetDeviceTokensByUserID(grpcCtx, req)
		if err != nil {
			logx.Errorf("SubscribeMsg_GetDeviceTokensByUserID_err :%+v", err)
			return
		}
		// ????????????
		if len(resp.UserDeviceTokens) <= 0 {
			return
		}
		tokens := make([]string, 0, len(resp.UserDeviceTokens))
		for _, tokenList := range resp.UserDeviceTokens {
			if len(tokenList.Values) < 0 {
				continue
			}
			tokens = append(tokens, tokenList.Values...)
		}
		if len(tokens) <= 0 {
			return
		}
		bodyMsg := new(model.MsgReq)
		bodyMsg.Msg = data
		bodyMsg.DeviceTokens = tokens
		// ??????topic
		sendMsgData, err := sonic.Marshal(bodyMsg)
		if err != nil {
			logx.Errorf("SubscribeMsg_Marshal_bodyMsg_err :%+v", err)
			return
		}
		writeCtx, cancel := context.WithTimeout(context.Background(), time.Second*10)
		defer cancel()
		w.producer.ProductMsg(writeCtx, sendMsgData, func(id pulsar.MessageID, message *pulsar.ProducerMessage, callBackErr error) {
			if err != nil {
				logx.Errorf("SubscribeMsg_ProductMsg_err :%+v", callBackErr)
				return
			}
		})
	})
}
