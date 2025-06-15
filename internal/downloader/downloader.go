package downloader

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/adshao/go-binance/v2"
)

// KlineDownloader 用于从币安下载K线数据
type KlineDownloader struct {
	client *binance.Client
}

// NewKlineDownloader 创建一个新的下载器实例
func NewKlineDownloader() *KlineDownloader {
	return &KlineDownloader{
		client: binance.NewClient("", ""), // 公共接口不需要API Key
	}
}

// DownloadKlines 下载指定交易对和时间范围内的1分钟K线数据，并保存到CSV文件
// 如果文件已存在，则会跳过下载，直接使用缓存。
func (d *KlineDownloader) DownloadKlines(symbol, filePath string, startTime, endTime time.Time) error {
	// 检查文件是否已存在（缓存）
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		fmt.Printf("从缓存加载数据: %s\n", filePath)
		return nil // 文件已存在，直接返回
	}

	fmt.Printf("开始下载 %s 从 %s 到 %s 的K线数据...\n", symbol, startTime.Format("2006-01-02"), endTime.Format("2006-01-02"))

	// --- 修复: 在创建文件前确保目录存在 ---
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("无法创建目录 %s: %v", dir, err)
	}
	// --- 修复结束 ---

	file, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("无法创建文件 %s: %v", filePath, err)
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	// 写入CSV表头
	header := []string{"open_time", "open", "high", "low", "close", "volume", "close_time", "quote_asset_volume", "number_of_trades", "taker_buy_base_asset_volume", "taker_buy_quote_asset_volume"}
	if err := writer.Write(header); err != nil {
		return fmt.Errorf("写入CSV表头失败: %v", err)
	}

	for t := startTime; t.Before(endTime); {
		klines, err := d.client.NewKlinesService().
			Symbol(symbol).
			Interval("1m").
			StartTime(t.UnixMilli()).
			Limit(1000). // 币安单次请求最多1000条
			Do(context.Background())

		if err != nil {
			return fmt.Errorf("下载K线数据失败: %v", err)
		}

		if len(klines) == 0 {
			break
		}

		for _, k := range klines {
			record := []string{
				fmt.Sprintf("%d", k.OpenTime),
				k.Open,
				k.High,
				k.Low,
				k.Close,
				k.Volume,
				fmt.Sprintf("%d", k.CloseTime),
				k.QuoteAssetVolume,
				fmt.Sprintf("%d", k.TradeNum),
				k.TakerBuyBaseAssetVolume,
				k.TakerBuyQuoteAssetVolume,
			}
			if err := writer.Write(record); err != nil {
				return fmt.Errorf("写入CSV记录失败: %v", err)
			}
		}

		// 更新下一次请求的开始时间
		t = time.UnixMilli(klines[len(klines)-1].CloseTime + 1)
		fmt.Printf("已下载数据至 %s\n", t.Format("2006-01-02 15:04:05"))
		time.Sleep(200 * time.Millisecond) // 避免过于频繁的请求
	}

	fmt.Printf("成功下载K线数据到 %s\n", filePath)
	return nil
}
