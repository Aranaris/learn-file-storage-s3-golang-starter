package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	
	videoIDString := r.PathValue("videoID")

	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	videoData, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't retrieve video data", err)
		return
	}

	videoDataWithPresignedURL, err := cfg.dbVideoToSignedVideo(videoData)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to generate presigned URL for video", err)
		return
	} 

	if userID != videoDataWithPresignedURL.UserID {
		respondWithError(w, http.StatusUnauthorized, "Invalid user upload", err)
		return
	}

	uploadLimit := 1 << 30
	err = r.ParseMultipartForm(int64(uploadLimit))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse form data", err)
		return
	}

	videoFile, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse video data", err)
		return
	}

	defer videoFile.Close()

	videoMediaType := header.Header.Get("Content-Type")
	if videoMediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid video format", err)
		return
	}

	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't process video file", err)
		return
	}

	_, err = io.Copy(tempFile, videoFile)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't save video file", err)
		return
	}

	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update seek start for tempfile", err)
		return
	}

	processedFilePath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't process video for fast start", err)
		return
	}

	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't read process video", err)
		return
	}

	os.Remove(tempFile.Name())
	tempFile.Close()

	defer os.Remove(processedFile.Name())
	defer processedFile.Close()

	ratio, err := getVideoAspectRatio(processedFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video aspect ratio", err)
		return
	}
	
	var prefix string
	if ratio == "16:9" {
		prefix = "landscape/"
	} else if ratio == "9:16" {
		prefix = "portrait/"
	} else {
		prefix = "other/"
	}


	fileName := make([]byte, 32)
	_, err = rand.Read(fileName)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate bucket filename", err)
		return
	}

	b := prefix + base64.RawURLEncoding.EncodeToString(fileName) + ".mp4"

	obj := s3.PutObjectInput{
		Bucket: &cfg.s3Bucket,
		Key: &b,
		Body: processedFile,
		ContentType: &videoMediaType,
	}
	
	_, err = cfg.s3Client.PutObject(context.TODO(), &obj)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't save video to s3", err)
		return
	}

	videoURL := fmt.Sprintf("%s,%s", cfg.s3Bucket, b)
	videoDataWithPresignedURL.VideoURL = &videoURL

	err = cfg.db.UpdateVideo(videoDataWithPresignedURL)

	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video URL", err)
		return
	}

}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL != nil {
		dbURLParams := strings.Split(*video.VideoURL, ",")

		if len(dbURLParams) < 2 {
			return video, fmt.Errorf("incorrect videoURL format saved in database")
		}
		bucket := dbURLParams[0]
		key := dbURLParams[1]
	
		presignedURL, err := generatePresignedURL(cfg.s3Client, bucket, key, time.Minute)
		if err != nil {
			return video, err
		}
	
		video.VideoURL = &presignedURL
	}
	
	return video, nil
}


func getVideoAspectRatio(filepath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filepath)
	var out bytes.Buffer
	cmd.Stdout = &out

	err := cmd.Run()
	if err != nil {
		return "", err
	}

	var videoData struct {
		Streams []struct {
		Width int `json:"width"`
		Height int `json:"height"` 
		} `json:"streams"`	
	}

	if err := json.Unmarshal([]byte(out.String()), &videoData); err != nil {
		return "", err
	}

	width := videoData.Streams[0].Width
	height := videoData.Streams[0].Height
	x := gcd(width, height)
	aspectRatio := fmt.Sprintf("%d:%d", width/x, height/x)
	ratio := float32(width) / float32(height)
	if aspectRatio == "16:9" || (ratio > (float32(16) / float32(9)) - .01 && ratio < (float32(16) / float32(9)) + .01) {
		return "16:9", nil
	}
	if aspectRatio == "9:16" || (ratio > (float32(9) / float32(16)) - .01 && ratio < (float32(9) / float32(16)) + .01) {
		return "9:16", nil
	}
	return "other", nil
}

func gcd(a int, b int) int{
	if b == 0 {
		return a
	}
	return gcd(b, a%b)
}

func processVideoForFastStart(filepath string) (string, error) {
	outputFilepath := filepath + ".processing"
	cmd := exec.Command("ffmpeg", "-i", filepath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputFilepath)
	if err := cmd.Run(); err != nil {
		return "", err
	}

	return outputFilepath, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	presignClient := s3.NewPresignClient(s3Client)
	obj := s3.GetObjectInput{
		Bucket: &bucket,
		Key: &key,
	}

	presignObject, err := presignClient.PresignGetObject(context.Background(), &obj, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", err
	}

	return presignObject.URL, nil
}
