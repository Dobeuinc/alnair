# Workloads Generation

## Testing with Deep Learning Training workloads

Test pod is created with [tensorflow/tensorflow:latest-gpu](https://hub.docker.com/r/tensorflow/tensorflow) image. An example training job scripts ([resnet-cifar10.py]()) is copied to the /tmp/scripts folder.

To run the workloads, first, create the test pod in your K8s cluster.

```kubectl apply -f dlt-job.yaml``` 

Then launch the training job with the following command. CIFAR10 data will be downloaded online during the first time execution.

```kubectl exec dlt-workload -- python3 resnet-cifar10.py```

To test more workloads, training scripts can be copied from local directory into the pod and lanuch in the same way. You can also modify the dockerfile and yaml file correspondingly.

```kubectl cp XXX.py dlt-workload:/tmp/scripts```

```kubectl exec dlt-workload -- python3 XXX.py```

In our environment, ImageNet data is preprocessed as tf record format and stored in network drive, and mounted to the container.
To run resnet imagenet training job inside container with the following command. Please modify the data set path in the dlt-job.yaml as needed. 
```
python3 /models/official/vision/image_classification/resnet/resnet_ctl_imagenet_main.py \
--data_dir=/tmp/data \
--model_dir=/tmp/model \
--num_gpus=2 \
--batch_size=64 \
--train_epochs=10 \
--steps_per_loop=100 \
--skip_eval=true \
--enable_eager=true
```
The above command is also saved in ```resnet_imagenet.sh``` file, which can be executed as following.

```kubectl exec dlt-workload -- bash resnet_imagenet.sh```