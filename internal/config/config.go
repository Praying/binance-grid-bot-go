package config

import (
	"binance-grid-bot-go/internal/models"
	"encoding/json"
	"os"
)

// LoadConfig 从指定路径加载JSON配置文件并解析到Config结构体中
func LoadConfig(path string) (*models.Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	config := &models.Config{}
	err = decoder.Decode(config)
	if err != nil {
		return nil, err
	}

	return config, nil
}
