package main

import (
	"net/http"
	_ "net/http/pprof"
	"os"
	"path/filepath"

	log "github.com/Sirupsen/logrus"
	"github.com/ensonmj/elise/cmd/elise/app"
	"github.com/rifflock/lfshook"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	fVerbose  int
	fLogDir   string
	fFlushLog bool
)

var mainCmd = &cobra.Command{
	Use:   "elise",
	Short: "Elise crawl webpage based on javascript, then parse and demonstrate",
	Long:  "Elise, the queue of spiders, one of the heroes of game League of Legends(LOL).",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if _, err := os.Stat(fLogDir); os.IsNotExist(err) {
			os.Mkdir(fLogDir, os.ModePerm)
		} else if fFlushLog {
			dir, err := os.Open(fLogDir)
			if err != nil {
				return err
			}
			defer dir.Close()
			names, err := dir.Readdirnames(-1)
			if err != nil {
				return err
			}
			for _, name := range names {
				err = os.RemoveAll(filepath.Join(fLogDir, name))
				if err != nil {
					return err
				}
			}
		}
		log.AddHook(lfshook.NewHook(lfshook.PathMap{
			log.DebugLevel: filepath.Join(fLogDir, "debug.log"),
			log.InfoLevel:  filepath.Join(fLogDir, "info.log"),
			log.WarnLevel:  filepath.Join(fLogDir, "warn.log"),
			log.FatalLevel: filepath.Join(fLogDir, "fatal.log"),
		}))
		log.SetLevel(log.Level(fVerbose))
		return nil
	},
}

func init() {
	mainCmd.AddCommand(app.VersionCmd)
	mainCmd.AddCommand(app.CrawlCmd)
	mainCmd.AddCommand(app.PicCmd)
	mainCmd.AddCommand(app.WebCmd)

	pflags := mainCmd.PersistentFlags()
	pflags.IntVarP(&fVerbose, "verbose", "v", 4, "log level: 0~5, 5 for debug detail")
	pflags.StringVar(&fLogDir, "logDir", "./log", "dir for storage log")
	pflags.BoolVar(&fFlushLog, "flushLog", false, "flush log dir for debug")
	viper.BindPFlag("verbose", pflags.Lookup("verbose"))
	viper.BindPFlag("logDir", pflags.Lookup("logDir"))
	viper.BindPFlag("flushLog", pflags.Lookup("flushLog"))
}

func main() {
	// /debug/pprof for profile
	go func() {
		http.ListenAndServe("127.0.0.1:5196", nil)
	}()

	viper.SetEnvPrefix("ELISE")
	viper.AutomaticEnv()
	viper.AddConfigPath("./conf")
	viper.SetConfigName("config")
	//viper.WatchConfig() // watching and re-reading config file
	err := viper.ReadInConfig()
	if err != nil {
		panic(err)
	}

	if err := mainCmd.Execute(); err != nil {
		log.WithError(err).Fatal("Elise exit")
		os.Exit(-1)
	}
}