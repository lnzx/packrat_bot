package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/message/styling"
	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/tg"
)

func main() {
	// 1. 创建一个分发器 (Dispatcher)
	// 这是处理 Telegram 推送过来的所有事件的核心
	dispatcher := tg.NewUpdateDispatcher()

	// 2. 在 Options 中配置这个分发器
	opts := telegram.Options{
		UpdateHandler: dispatcher,
	}

	if err := telegram.BotFromEnvironment(
		context.Background(),
		opts,
		nil, // setup 不需要可以传 nil
		func(ctx context.Context, client *telegram.Client) error {
			bot := NewBot(client.API())

			dispatcher.OnNewMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
				msg, ok := u.Message.(*tg.Message)
				if !ok || msg.Message == "" || msg.Out {
					return nil
				}

				// 👇 拦截 /start 命令
				if msg.Message == "/start" {
					_, err := bot.sender.Answer(e, u).Text(ctx,
						"👋 欢迎使用本机器人！\n\n"+
							"📖 使用说明：\n"+
							"直接发送 Telegram 消息链接即可转发内容。\n\n"+
							"支持的链接格式：\n"+
							"• https://t.me/频道名/消息ID\n"+
							"• https://t.me/频道名/消息ID?single\n",
					)
					return err
				}

				taskCtx := context.WithoutCancel(ctx)
				go func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("Recovered from panic in goroutine: %v\n", r)
						}
					}()

					if err := bot.handleForwardRequest(taskCtx, e, u); err != nil {
						log.Printf("处理转发请求失败: %v\n", err)
					}
				}()

				return nil
			})

			log.Println("机器人已启动，正在监听消息...")
			<-ctx.Done()
			return ctx.Err()
		},
	); err != nil {
		log.Fatal(err)
	}
}

type Bot struct {
	api     *tg.Client
	sender  *message.Sender
	manager *peers.Manager
}

func NewBot(api *tg.Client) *Bot {
	return &Bot{
		api:     api,
		sender:  message.NewSender(api),
		manager: peers.Options{}.Build(api),
	}
}

func (b *Bot) handleForwardRequest(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
	// 1. 解析链接
	channelUsername, msgID, err := parseTelegramLink(u.Message.(*tg.Message).Message)
	if err != nil {
		// 👇 新增：解析失败时，告诉用户链接格式不对
		_, _ = b.sender.Answer(e, u).Text(ctx, "解析失败，请发送正确的 Telegram 消息链接！")
		return err
	}

	// 2. 获取目标频道的 Peer 信息 (类似解析 username)
	peer, err := b.manager.Resolve(ctx, channelUsername)
	if err != nil {
		return fmt.Errorf("无法解析频道: %w", err)
	}

	// 2.1 将通用的 Peer 断言为具体的 Channel 类型
	channelPeer, ok := peer.(peers.Channel)
	if !ok {
		// 如果断言失败，说明这个 username 是一个人或者普通群，不是频道
		return fmt.Errorf("实体类型错误: %s 不是一个频道或超级群", channelUsername)
	}

	// 3. 获取信息
	res, err := b.getMessages(ctx, channelPeer, []int{msgID})
	if err != nil {
		return err
	}

	targetMsg, err := b.unpackFirstMessage(res)
	if err != nil {
		return err
	}

	// 4. 🎉 完美走到这里，msg 已经是纯正的 *tg.Message 了！
	log.Println("成功拿到普通消息！内容: ", targetMsg.Message)

	// -----------------------------------------
	// 第一步：先抓“相册/图集”这个特殊情况 (最复杂的边界情况)
	// -----------------------------------------
	if targetMsg.GroupedID != 0 {
		return b.handleAlbum(ctx, e, u, channelPeer, targetMsg)
	}

	// -----------------------------------------
	// 第二步：处理单文件媒体 (核心的秒传逻辑)
	// -----------------------------------------
	if targetMsg.Media != nil {
		return b.handleMedia(ctx, e, u, targetMsg)
	}

	// 第三步：处理纯文本 (最基础的情况)
	// 能走到这里，说明 msg.GroupedID == 0 且 msg.Media == nil
	_, err = b.sender.Answer(e, u).Text(ctx, targetMsg.Message)
	return err
}

func (b *Bot) getMessages(ctx context.Context, channelPeer peers.Channel, ids []int) (tg.MessagesMessagesClass, error) {
	var inputIDs []tg.InputMessageClass
	for _, id := range ids {
		inputIDs = append(inputIDs, &tg.InputMessageID{ID: id})
	}

	// 2. 拿着频道 Peer 和 MessageID 去请求消息详情 (1:1 对应官方文档的 channels.getMessages)
	res, err := b.api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: channelPeer.InputChannel(),
		ID:      inputIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("获取消息失败: %w", err)
	}
	return res, nil
}

func (b *Bot) unpackFirstMessage(res tg.MessagesMessagesClass) (*tg.Message, error) {
	// 3.1 使用 AsModified() 脱去外层包裹（不需要写繁琐的 switch 去管它是哪种包裹）
	modified, ok := res.AsModified()
	if !ok || len(modified.GetMessages()) == 0 {
		// 只有在触发高级缓存 (NotModified) 时才会进这里，单条请求通常不会触发
		return nil, fmt.Errorf("服务器没有返回有效的包裹")
	}

	// 4. 因为只请求了 1 个 ID，所以直接取数组第 0 个元素
	// 【关键！】使用 switch 断言具体的 MessageClass 类型
	// 【核心极简写法】直接进行类型断言
	targetMsg, ok := modified.GetMessages()[0].(*tg.Message)
	if !ok {
		// 只要进到这里，说明它要么是 MessageEmpty (被删了)
		// 要么是 MessageService (系统消息)，要么是其他奇葩类型。
		// 反正都不能克隆，直接抛出错误退出！
		return nil, fmt.Errorf("该消息无法被克隆 (可能是被删除、系统消息或不支持的类型)")
	}
	return targetMsg, nil
}

func (b *Bot) handleMedia(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage, targetMsg *tg.Message) error {
	var mediaOption message.MediaOption
	caption := styling.Plain(targetMsg.Message)

	switch media := targetMsg.Media.(type) {
	case *tg.MessageMediaPhoto:
		// 图片属于 Photo
		photo, ok := media.Photo.(*tg.Photo)
		if !ok {
			return fmt.Errorf("无法解析 Photo 类型")
		}
		mediaOption = message.Photo(photo.AsInput(), caption)
	case *tg.MessageMediaDocument:
		// 视频、GIF、普通文件都属于 Document
		doc, ok := media.Document.(*tg.Document)
		if !ok {
			return fmt.Errorf("无法解析 Document 类型")
		}
		// 使用 message.Document 包装底层 ID
		mediaOption = message.Document(doc.AsInput(), caption)
	default:
		_, _ = b.sender.Answer(e, u).Text(ctx, "❌ 暂不支持抓取该类型的媒体 (例如投票、位置等)。")
		return fmt.Errorf("不支持的媒体类型: %T", targetMsg.Media)
	}

	// 发送媒体，并附带文字
	_, err := b.sender.Answer(e, u).Media(ctx, mediaOption)
	return err // 处理完毕，退出
}

func (b *Bot) handleAlbum(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage, channelPeer peers.Channel, targetMsg *tg.Message) error {
	log.Printf("检测到GroupedID: %d，正在搜索关联媒体...\n", targetMsg.GroupedID)

	// 构造相邻的消息 ID 列表（前后各取 10 条，共 20 个 ID）
	var ids []int
	for i := targetMsg.ID - 10; i <= targetMsg.ID+10; i++ {
		if i > 0 {
			ids = append(ids, i)
		}
	}

	res, err := b.getMessages(ctx, channelPeer, ids)
	if err != nil {
		return fmt.Errorf("获取相册消息失败: %w", err)
	}

	modified, ok := res.AsModified()
	if !ok {
		return fmt.Errorf("无法解析相册消息返回格式")
	}

	// 筛选相同 GroupedID 的消息
	var messagesToGroup []*tg.Message
	for _, m := range modified.GetMessages() {
		if c, ok := m.(*tg.Message); ok {
			if c.GroupedID != 0 && c.GroupedID == targetMsg.GroupedID {
				messagesToGroup = append(messagesToGroup, c)
				log.Println("Grouped c id: ", c.ID)
			}
		}
	}

	if len(messagesToGroup) == 0 {
		return fmt.Errorf("获取到0条历史记录")
	}
	sort.Slice(messagesToGroup, func(i, j int) bool {
		return messagesToGroup[i].ID < messagesToGroup[j].ID
	})

	var multiMedia []message.MultiMediaOption
	for _, m := range messagesToGroup {
		// 提取文案 (如果有的话)
		caption := styling.Plain(m.Message)

		switch media := m.Media.(type) {
		case *tg.MessageMediaPhoto:
			if photo, ok := media.Photo.AsNotEmpty(); ok {
				// 使用简化的 AsInput() 获取发送凭据
				multiMedia = append(multiMedia, message.Photo(photo.AsInput(), caption))
			}
		case *tg.MessageMediaDocument:
			if doc, ok := media.Document.AsNotEmpty(); ok {
				// 自动处理视频、GIF 或文件
				multiMedia = append(multiMedia, message.Document(doc.AsInput(), caption))
			}
		}
	}
	if len(multiMedia) == 0 {
		return fmt.Errorf("专辑中没有可发送的媒体")
	}
	_, err = b.sender.Answer(e, u).Album(ctx, multiMedia[0], multiMedia[1:]...)
	return err
}

func parseTelegramLink(link string) (string, int, error) {
	u, err := url.Parse(link)
	if err != nil {
		return "", 0, err
	}

	host := strings.ToLower(u.Host)

	if host != "t.me" && host != "www.t.me" && host != "telegram.me" {
		return "", 0, fmt.Errorf("invalid host")
	}

	// 清理 path
	path := strings.Trim(u.Path, "/")

	parts := strings.Split(path, "/")

	// 支持 /s/a/123
	if len(parts) >= 3 && parts[0] == "s" {
		parts = parts[1:]
	}

	if len(parts) < 2 {
		return "", 0, fmt.Errorf("invalid path")
	}

	channel := parts[0]

	id, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", 0, fmt.Errorf("invalid message id")
	}

	return channel, id, nil
}
