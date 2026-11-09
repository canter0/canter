// canter-sandbox-image builds the prebuilt sandbox base image: a temporary
// 1x1 VM installs the headless package set, cleans itself for imaging, and
// powers off; it is then snapshotted and the build VM is deleted.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/canter0/canter/internal/provider/compute"
	"github.com/canter0/canter/internal/wrencomputer"
)

func main() {
	version := flag.String("version", "1", "image build version stamped into /opt/canter/sandbox-image-version")
	flag.Parse()

	client, err := compute.NewFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	shape, imageID, networks, err := client.ResolveSize(ctx, 1, 1024, 10, "ubuntu-24.04")
	if err != nil {
		log.Fatal(err)
	}
	server, err := client.Create(ctx, "canter-sandbox-image-build", "canter-sandbox-image-build", shape.ID, imageID, networks[0], wrencomputer.ImageBuildBootstrap(*version))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("build vm:", server.ID)
	defer func() {
		// The build VM must never linger, even on failure.
		for i := 0; i < 6; i++ {
			if err := client.Delete(context.Background(), server.ID); err == nil {
				return
			}
			time.Sleep(5 * time.Second)
		}
	}()

	if _, err = client.WaitActive(ctx, server.ID); err != nil {
		log.Fatal(err)
	}
	fmt.Println("vm active; waiting for bootstrap + poweroff")
	for {
		current, err := client.Server(ctx, server.ID)
		if err != nil {
			log.Fatal(err)
		}
		if current.Status == "SHUTOFF" {
			break
		}
		if current.Status == "ERROR" {
			log.Fatal("build vm entered ERROR")
		}
		time.Sleep(10 * time.Second)
	}

	name := fmt.Sprintf("canter-sandbox-%s-%d", time.Now().UTC().Format("20060102"), time.Now().Unix()%100000)
	image, err := client.CreateImage(ctx, server.ID, name, map[string]string{"canter.managed": "true", "canter.kind": "sandbox-image", "canter.image-version": *version})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("image:", image, name)
	for {
		status, err := client.ImageStatus(ctx, image)
		if err != nil {
			log.Fatal(err)
		}
		if status == "active" {
			break
		}
		if status == "deleted" || status == "error" {
			log.Fatalf("image entered %s", status)
		}
		time.Sleep(10 * time.Second)
	}
	fmt.Println("image active:", image)
	if err := client.Delete(ctx, server.ID); err != nil {
		log.Fatal(err)
	}
	if err := waitGone(ctx, client, server.ID); err != nil {
		log.Fatal(err)
	}
	fmt.Println("build vm deleted")
	fmt.Printf("DONE set CANTER_SANDBOX_IMAGE=%s\n", image)
}

// waitGone polls until the provider confirms the build VM is deleted.
func waitGone(ctx context.Context, client *compute.Client, id string) error {
	deadline := time.Now().Add(3 * time.Minute)
	for {
		_, err := client.Server(ctx, id)
		if compute.IsNotFound(err) {
			return nil
		}
		if err == nil {
			if time.Now().After(deadline) {
				return fmt.Errorf("build vm %s still exists after deletion", id)
			}
			time.Sleep(3 * time.Second)
			continue
		}
		return err
	}
}
