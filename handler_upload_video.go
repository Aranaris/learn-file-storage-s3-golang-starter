package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
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

	if userID != videoData.UserID {
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

	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	_, err = io.Copy(tempFile, videoFile)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't save video file", err)
		return
	}

	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update save file", err)
		return
	}

	fileName := make([]byte, 32)
	_, err = rand.Read(fileName)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate bucket filename", err)
		return
	}

	b := base64.RawURLEncoding.EncodeToString(fileName) + ".mp4"

	obj := s3.PutObjectInput{
		Bucket: &cfg.s3Bucket,
		Key: &b,
		Body: tempFile,
		ContentType: &videoMediaType,
	}
	
	_, err = cfg.s3Client.PutObject(context.TODO(), &obj)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't save video to s3", err)
		return
	}

	videoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, b)
	videoData.VideoURL = &videoURL

	err = cfg.db.UpdateVideo(videoData)

	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video URL", err)
		return
	}

}
