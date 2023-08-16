package main

import (
	"container/list"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"testing"
	"time"

	"github.com/cbergoon/merkletree"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

func TestGoCacheStoreMerkleTree(t *testing.T) {

	conf, delegate, events = GetConf()

	store = NewGoCacheStore()
	defer store.Close()

	for i := 0; i < NumTestValues; i++ {
		store.setValue(testValue(fmt.Sprintf("keyz%d", i), fmt.Sprintf("value%d", i)))
	}

	startTime := time.Now()

	tree1, err := PartitionMerkleTree(1, 1)
	if err != nil {
		t.Error(err)
	}

	_, err = tree1.VerifyTree()
	if err != nil {
		t.Error(err)
	}

	tree2, err := PartitionMerkleTree(1, 1)
	if err != nil {
		t.Error(err)
	}

	err = tree1.RebuildTree()
	if err != nil {
		t.Error(err)
	}

	err = tree2.RebuildTree()
	if err != nil {
		t.Error(err)
	}

	assert.EqualValues(t, tree1.Root.Hash, tree2.Root.Hash, "Tree hashes don't match")

	elapsedTime := time.Since(startTime).Seconds()

	fmt.Printf("GoCache Elapsed Time: %.2f seconds\n", elapsedTime)

}

func TestLevelDbStoreMerkleTree(t *testing.T) {
	hostname = randomString(5)

	conf, delegate, events = GetConf()

	store = NewLevelDbStore()
	defer store.Close()

	for i := 0; i < NumTestValues; i++ {
		store.setValue(testValue(fmt.Sprintf("keyz%d", i), fmt.Sprintf("value%d", i)))
	}

	startTime := time.Now()

	tree1, err := PartitionMerkleTree(1, 1)
	if err != nil {
		t.Error(err)
	}

	_, err = tree1.VerifyTree()
	if err != nil {
		t.Error(err)
	}

	tree2, err := PartitionMerkleTree(1, 1)
	if err != nil {
		t.Error(err)
	}

	err = tree1.RebuildTree()
	if err != nil {
		t.Error(err)
	}

	err = tree2.RebuildTree()
	if err != nil {
		t.Error(err)
	}

	assert.EqualValues(t, tree1.Root.Hash, tree2.Root.Hash, "Tree hashes don't match")
	elapsedTime := time.Since(startTime).Seconds()

	fmt.Printf("LevelDb Elapsed Time: %.2f seconds\n", elapsedTime)
}

func BFS(root *merkletree.Node) []*merkletree.Node {
	if root == nil {
		return nil
	}

	queue := list.New()
	queue.PushBack(root)

	var result []*merkletree.Node

	for queue.Len() > 0 {
		current := queue.Remove(queue.Front()).(*merkletree.Node)
		result = append(result, current)

		if current.Left != nil {
			queue.PushBack(current.Left)
		}

		if current.Right != nil {
			queue.PushBack(current.Left)
		}
	}

	return result
}

// TestContent implements the Content interface provided by merkletree and represents the content stored in the tree.
type TestContent struct {
	x string
}

// CalculateHash hashes the values of a TestContent
func (t TestContent) CalculateHash() ([]byte, error) {
	h := sha256.New()
	if _, err := h.Write([]byte(t.x)); err != nil {
		return nil, err
	}

	return h.Sum(nil), nil
}

// Equals tests for equality of two Contents
func (t TestContent) Equals(other merkletree.Content) (bool, error) {
	otherTC, ok := other.(TestContent)
	if !ok {
		return false, errors.New("value is not of type TestContent")
	}
	return t.x == otherTC.x, nil
}

func TestMerkleTree(t *testing.T) {
	return
	var list1 []merkletree.Content
	for i := 0; i < 10; i++ {
		list1 = append(list1, TestContent{x: "Hello"})
		list1 = append(list1, TestContent{x: "Hi"})
		list1 = append(list1, TestContent{x: "Hey"})
		list1 = append(list1, TestContent{x: "Hola"})
		list1 = append(list1, TestContent{x: "Hello"})
	}

	// Create a new Merkle Tree from the list of Content
	tree1, err := merkletree.NewTree(list1)
	if err != nil {
		log.Fatal(err)
	}

	var list2 []merkletree.Content

	for i := 0; i < 10; i++ {
		list2 = append(list2, TestContent{x: "Hello"})
		list2 = append(list2, TestContent{x: "Hi"})
		list2 = append(list2, TestContent{x: "Hey"})
		list2 = append(list2, TestContent{x: "Hola"})
		list2 = append(list2, TestContent{x: "Hello"})
	}

	list2 = append(list2, TestContent{x: "Hello"})

	// Create a new Merkle Tree from the list of Content
	tree2, err := merkletree.NewTree(list2)
	if err != nil {
		log.Fatal(err)
	}

	nodes1 := BFS(tree1.Root)
	nodes2 := BFS(tree2.Root)

	// diff := 0
	// for i, _ := range nodes1 {
	// 	logrus.Info("compare ", bytes.Compare(nodes1[i].Hash, nodes2[i].Hash))
	// 	diff += bytes.Compare(nodes1[i].Hash, nodes2[i].Hash)
	// 	// logrus.Info(node.C)
	// 	// logrus.Info("hash ", node.Hash)
	// 	// logrus.Info(tree1.VerifyContent(node.C))
	// }
	// logrus.Info("diff ", diff)

	logrus.Info("len1 ", len(nodes1))

	logrus.Info("len2 ", len(nodes2))

	logrus.Info(tree1.VerifyContent(TestContent{x: "Hello"}))

	logrus.Info(tree1.Root.C)
	logrus.Info(len(nodes1))
	// tree1.VerifyContent(verifyList)

	// assert.EqualValues(t, tree1.Root.Hash, tree2.Root.Hash, "Tree hashes don't match")
}