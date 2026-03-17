package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/message/styling"
	"github.com/gotd/td/tg"
)

func main() {
	appId, err := strconv.Atoi(os.Getenv("TG_API_ID"))
	if err != nil {
		log.Fatalln(err)
	}
	appHash := os.Getenv("TG_API_HASH")
	botToken := os.Getenv("TG_BOT_TOKEN")

	if appId == 0 || appHash == "" || botToken == "" {
		log.Fatalln("TG_API_ID , TG_API_HASH, TG_BOT_TOKEN are required")
		return
	}

	// 1. 创建一个分发器 (Dispatcher)
	// 这是处理 Telegram 推送过来的所有事件的核心
	dispatcher := tg.NewUpdateDispatcher()

	// 3. 在 Options 中配置这个分发器
	opts := telegram.Options{
		UpdateHandler: dispatcher,
	}

	client := telegram.NewClient(appId, appHash, opts)
	// 2. 获取 API 句柄
	api := client.API()
	// 新增：初始化一个高层级的消息发送器 Sender
	sender := message.NewSender(api)
	// 处理私聊或普通群组消息
	dispatcher.OnNewMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
		// 1. 快速过滤无效消息（同步执行，不占资源）
		msg, ok := u.Message.(*tg.Message)
		if !ok || msg.Message == "" {
			return nil
		}

		// 2. 启动异步协程
		// 使用 WithoutCancel 确保 OnNewMessage 返回后，协程不会被掐断
		taskCtx := context.WithoutCancel(ctx)

		go func() {
			// 建议在协程内部增加 recover，防止由于逻辑 bug 导致整个 Bot 崩溃
			defer func() {
				if r := recover(); r != nil {
					log.Printf("Recovered from panic in goroutine: %v", r)
				}
			}()

			// 执行具体的业务逻辑
			if err = handleLinkRequest(taskCtx, api, sender, e, u, msg); err != nil {
				log.Printf("处理链接请求失败: %v", err)
			}
		}()

		// 3. 立即返回，让 Dispatcher 去处理下一条别人的消息
		return nil
	})

	// 4. 运行客户端
	if err = client.Run(context.Background(), func(ctx context.Context) error {
		// It is only valid to use client while this function is not returned and ctx is not cancelled.
		// 如果运行到这里，说明网络已经通了
		log.Println("--- 成功连接到 Telegram 服务器 ---")

		// 登录（Bot 或 UserBot）
		if _, err = client.Auth().Bot(ctx, botToken); err != nil {
			return err
		}

		log.Println("机器人已启动，正在监听消息...")

		// 阻塞运行，等待上下文结束
		<-ctx.Done()
		return ctx.Err()
	}); err != nil {
		log.Fatal(err)
	}
	// Client is closed.
}

// handleLinkRequest 抽离出的业务逻辑函数
func handleLinkRequest(ctx context.Context, api *tg.Client, sender *message.Sender, e tg.Entities, u *tg.UpdateNewMessage, msg *tg.Message) error {
	// 1. 解析链接
	channelUsername, msgID, err := parseTelegramLink(msg.Message)
	if err != nil {
		// 👇 新增：解析失败时，告诉用户链接格式不对
		_, _ = sender.Answer(e, u).Text(ctx, "解析失败，请发送正确的 Telegram 消息链接！")
		return err
	}

	// 2. 解析目标频道用户名，获取 AccessHash 等鉴权信息
	resolved, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: channelUsername})
	if err != nil {
		_, _ = sender.Answer(e, u).Text(ctx, "❌ 无法解析该频道，请确保频道是公开的。")
		return err
	}

	var targetChannel *tg.InputChannel
	for _, chat := range resolved.GetChats() {
		if c, ok := chat.(*tg.Channel); ok {
			targetChannel = &tg.InputChannel{
				ChannelID:  c.ID,
				AccessHash: c.AccessHash,
			}
			break
		}
	}

	if targetChannel == nil {
		_, _ = sender.Answer(e, u).Text(ctx, "❌ 找不到该频道的信息。")
		return err
	}

	// 3. 请求特定的消息内容
	msgsClass, err := api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: targetChannel,
		ID: []tg.InputMessageClass{
			&tg.InputMessageID{ID: msgID},
		},
	})
	if err != nil {
		_, _ = sender.Answer(e, u).Text(ctx, "❌ 获取消息失败。")
		return err
	}

	// gotd 提供的 Helper: 提取真实的消息切片
	modified, ok := msgsClass.AsModified()
	if !ok || len(modified.GetMessages()) == 0 {
		_, _ = sender.Answer(e, u).Text(ctx, "❌ 未找到该消息，可能已被删除。")
		return err
	}

	targetMsg, ok := modified.GetMessages()[0].(*tg.Message)
	if !ok {
		_, _ = sender.Answer(e, u).Text(ctx, "❌ 不支持的消息类型 (可能是服务消息)。")
		return err
	}

	// 4. 提取视频/文件/图片的“身份证”
	if targetMsg.Media == nil {
		_, _ = sender.Answer(e, u).Text(ctx, "✅ 抓取成功，但这是一条纯文本消息：\n\n"+targetMsg.Message)
		return err
	}

	var mediaOption message.MediaOption
	caption := targetMsg.Message

	// Telegram 中的多态：根据具体的媒体类型进行转换
	switch media := targetMsg.Media.(type) {
	case *tg.MessageMediaDocument:
		// 视频、GIF、普通文件都属于 Document
		doc, ok := media.Document.(*tg.Document)
		if !ok {
			return err
		}
		// 使用 message.Document 包装底层 ID
		mediaOption = message.Document(&tg.InputDocument{
			ID:            doc.ID,
			AccessHash:    doc.AccessHash,
			FileReference: doc.FileReference,
		}, styling.Plain(caption))

	case *tg.MessageMediaPhoto:
		// 图片属于 Photo
		photo, ok := media.Photo.(*tg.Photo)
		if !ok {
			return err
		}
		// 使用 message.Photo 包装底层 ID
		mediaOption = message.Photo(&tg.InputPhoto{
			ID:            photo.ID,
			AccessHash:    photo.AccessHash,
			FileReference: photo.FileReference,
		}, styling.Plain(caption))

	default:
		_, _ = sender.Answer(e, u).Text(ctx, "❌ 暂不支持抓取该类型的媒体 (例如投票、位置等)。")
		return err
	}

	// 发送媒体，并附带文字
	_, err = sender.Answer(e, u).Media(ctx, mediaOption)

	if err != nil {
		_, _ = sender.Answer(e, u).Text(ctx, "❌ 发送文件失败，请查看日志。")
		return err
	}

	log.Printf("✅ 成功抓取频道 %s 的消息 %d 并发送给用户！", channelUsername, msgID)
	return nil
}

// parseTelegramLink 从 Telegram 消息链接提取频道用户名和消息ID
func parseTelegramLink(messageLink string) (channelUsername string, messageID int, err error) {
	// 解析 URL
	parsedURL, err := url.Parse(messageLink)
	if err != nil {
		return "", 0, err
	}

	// 检查主机是否为 t.me
	if parsedURL.Host != "t.me" {
		return "", 0, fmt.Errorf("invalid Telegram URL")
	}

	// path 是 /<channel_username>/<message_id>
	parts := strings.Split(strings.Trim(parsedURL.Path, "/"), "/")
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("URL format invalid")
	}

	channelUsername = parts[0]

	// 将消息 ID 转换为整数
	messageID, err = strconv.Atoi(parts[1])
	if err != nil {
		return "", 0, fmt.Errorf("invalid message ID")
	}

	return channelUsername, messageID, nil
}
