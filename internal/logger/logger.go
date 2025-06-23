package logger

import (
	"binance-grid-bot-go/internal/models"
	"os"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

var sugaredLogger *zap.SugaredLogger

// InitLogger 初始化zap日志记录器
func InitLogger(cfg models.LogConfig) {
	// 配置日志级别
	logLevel := zap.NewAtomicLevel()
	if err := logLevel.UnmarshalText([]byte(cfg.Level)); err != nil {
		logLevel.SetLevel(zap.InfoLevel) // 默认为Info级别
	}

	// 配置encoder
	encoderConfig := zap.NewProductionEncoderConfig()
	encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	// 为控制台输出启用颜色
	encoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	consoleEncoder := zapcore.NewConsoleEncoder(encoderConfig)

	// 根据配置创建Cores
	var cores []zapcore.Core

	output := strings.ToLower(cfg.Output)
	if output == "file" || output == "both" {
		// 设置lumberjack进行日志切割
		lumberjackLogger := &lumberjack.Logger{
			Filename:   cfg.File,
			MaxSize:    cfg.MaxSize,
			MaxBackups: cfg.MaxBackups,
			MaxAge:     cfg.MaxAge,
			Compress:   cfg.Compress,
		}
		fileWriter := zapcore.AddSync(lumberjackLogger)
		cores = append(cores, zapcore.NewCore(consoleEncoder, fileWriter, logLevel))
	}

	if output == "console" || output == "both" {
		// 设置控制台输出
		consoleWriter := zapcore.AddSync(os.Stdout)
		cores = append(cores, zapcore.NewCore(consoleEncoder, consoleWriter, logLevel))
	}

	// 如果没有有效的core（例如配置错误），则默认输出到控制台
	if len(cores) == 0 {
		consoleWriter := zapcore.AddSync(os.Stdout)
		cores = append(cores, zapcore.NewCore(consoleEncoder, consoleWriter, logLevel))
	}

	// 创建Tee Core
	core := zapcore.NewTee(cores...)

	// 创建logger
	logger := zap.New(core, zap.AddCaller())
	sugaredLogger = logger.Sugar()
}

// S 返回全局的sugared logger实例
func S() *zap.SugaredLogger {
	if sugaredLogger == nil {
		// 如果logger未初始化，则提供一个默认的应急logger
		logger, _ := zap.NewDevelopment()
		return logger.Sugar()
	}
	return sugaredLogger
}
