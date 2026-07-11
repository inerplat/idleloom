#include <vulkan/vulkan.h>

#include <ctype.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#define VALUE_COUNT 64
#define SHADER_PATH "/usr/local/lib/apple-vulkan/probe.comp.spv"

static int contains_case_insensitive(const char *text, const char *needle) {
    size_t needle_len = strlen(needle);
    if (needle_len == 0) {
        return 1;
    }
    for (; *text != '\0'; text++) {
        size_t i = 0;
        while (i < needle_len && text[i] != '\0' &&
               tolower((unsigned char)text[i]) == tolower((unsigned char)needle[i])) {
            i++;
        }
        if (i == needle_len) {
            return 1;
        }
    }
    return 0;
}

static uint32_t find_memory_type(VkPhysicalDevice physical_device,
                                 uint32_t type_bits,
                                 VkMemoryPropertyFlags properties) {
    VkPhysicalDeviceMemoryProperties memory_properties;
    vkGetPhysicalDeviceMemoryProperties(physical_device, &memory_properties);
    for (uint32_t i = 0; i < memory_properties.memoryTypeCount; i++) {
        if ((type_bits & (1u << i)) != 0 &&
            (memory_properties.memoryTypes[i].propertyFlags & properties) == properties) {
            return i;
        }
    }
    return UINT32_MAX;
}

static uint32_t *read_shader(size_t *size_out) {
    FILE *file = fopen(SHADER_PATH, "rb");
    if (file == NULL) {
        fprintf(stderr, "open shader %s failed\n", SHADER_PATH);
        return NULL;
    }
    if (fseek(file, 0, SEEK_END) != 0) {
        fclose(file);
        return NULL;
    }
    long size = ftell(file);
    if (size <= 0 || size % 4 != 0 || fseek(file, 0, SEEK_SET) != 0) {
        fclose(file);
        return NULL;
    }
    uint32_t *code = malloc((size_t)size);
    if (code == NULL || fread(code, 1, (size_t)size, file) != (size_t)size) {
        free(code);
        fclose(file);
        return NULL;
    }
    fclose(file);
    *size_out = (size_t)size;
    return code;
}

static int fail_vk(const char *operation, VkResult result) {
    fprintf(stderr, "%s failed: VkResult=%d\n", operation, result);
    return 1;
}

int main(void) {
    if (getenv("VK_DRIVER_FILES") == NULL) {
        const char *candidates[] = {
            "/usr/share/vulkan/icd.d/virtio_icd.aarch64.json",
            "/usr/share/vulkan/icd.d/virtio_icd.json",
        };
        for (size_t i = 0; i < sizeof(candidates) / sizeof(candidates[0]); i++) {
            if (access(candidates[i], R_OK) == 0) {
                setenv("VK_DRIVER_FILES", candidates[i], 0);
                break;
            }
        }
    }
    VkResult result;
    VkApplicationInfo app_info = {
        .sType = VK_STRUCTURE_TYPE_APPLICATION_INFO,
        .pApplicationName = "apple-vulkan-probe",
        .applicationVersion = VK_MAKE_VERSION(0, 1, 0),
        .pEngineName = "none",
        .engineVersion = VK_MAKE_VERSION(0, 1, 0),
        .apiVersion = VK_API_VERSION_1_1,
    };
    VkInstanceCreateInfo instance_info = {
        .sType = VK_STRUCTURE_TYPE_INSTANCE_CREATE_INFO,
        .pApplicationInfo = &app_info,
    };
    VkInstance instance;
    result = vkCreateInstance(&instance_info, NULL, &instance);
    if (result != VK_SUCCESS) {
        return fail_vk("vkCreateInstance", result);
    }

    uint32_t physical_count = 0;
    result = vkEnumeratePhysicalDevices(instance, &physical_count, NULL);
    if (result != VK_SUCCESS || physical_count == 0) {
        vkDestroyInstance(instance, NULL);
        return fail_vk("vkEnumeratePhysicalDevices", result);
    }
    VkPhysicalDevice *physical_devices = calloc(physical_count, sizeof(*physical_devices));
    if (physical_devices == NULL) {
        vkDestroyInstance(instance, NULL);
        return 1;
    }
    result = vkEnumeratePhysicalDevices(instance, &physical_count, physical_devices);
    if (result != VK_SUCCESS) {
        free(physical_devices);
        vkDestroyInstance(instance, NULL);
        return fail_vk("vkEnumeratePhysicalDevices", result);
    }

    VkPhysicalDevice physical_device = VK_NULL_HANDLE;
    VkPhysicalDeviceProperties physical_properties;
    for (uint32_t i = 0; i < physical_count; i++) {
        VkPhysicalDeviceProperties candidate;
        vkGetPhysicalDeviceProperties(physical_devices[i], &candidate);
        if (contains_case_insensitive(candidate.deviceName, "venus")) {
            physical_device = physical_devices[i];
            physical_properties = candidate;
            break;
        }
    }
    free(physical_devices);
    if (physical_device == VK_NULL_HANDLE) {
        fprintf(stderr, "no Vulkan physical device with Venus in its name\n");
        vkDestroyInstance(instance, NULL);
        return 1;
    }

    uint32_t queue_count = 0;
    vkGetPhysicalDeviceQueueFamilyProperties(physical_device, &queue_count, NULL);
    VkQueueFamilyProperties *queue_properties = calloc(queue_count, sizeof(*queue_properties));
    if (queue_properties == NULL) {
        vkDestroyInstance(instance, NULL);
        return 1;
    }
    vkGetPhysicalDeviceQueueFamilyProperties(physical_device, &queue_count, queue_properties);
    uint32_t queue_family = UINT32_MAX;
    for (uint32_t i = 0; i < queue_count; i++) {
        if ((queue_properties[i].queueFlags & VK_QUEUE_COMPUTE_BIT) != 0) {
            queue_family = i;
            break;
        }
    }
    free(queue_properties);
    if (queue_family == UINT32_MAX) {
        fprintf(stderr, "Venus device has no compute queue\n");
        vkDestroyInstance(instance, NULL);
        return 1;
    }

    float queue_priority = 1.0f;
    VkDeviceQueueCreateInfo queue_info = {
        .sType = VK_STRUCTURE_TYPE_DEVICE_QUEUE_CREATE_INFO,
        .queueFamilyIndex = queue_family,
        .queueCount = 1,
        .pQueuePriorities = &queue_priority,
    };
    VkDeviceCreateInfo device_info = {
        .sType = VK_STRUCTURE_TYPE_DEVICE_CREATE_INFO,
        .queueCreateInfoCount = 1,
        .pQueueCreateInfos = &queue_info,
    };
    VkDevice device;
    result = vkCreateDevice(physical_device, &device_info, NULL, &device);
    if (result != VK_SUCCESS) {
        vkDestroyInstance(instance, NULL);
        return fail_vk("vkCreateDevice", result);
    }
    VkQueue queue;
    vkGetDeviceQueue(device, queue_family, 0, &queue);

    VkDeviceSize buffer_size = VALUE_COUNT * sizeof(uint32_t);
    VkBufferCreateInfo buffer_info = {
        .sType = VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO,
        .size = buffer_size,
        .usage = VK_BUFFER_USAGE_STORAGE_BUFFER_BIT,
        .sharingMode = VK_SHARING_MODE_EXCLUSIVE,
    };
    VkBuffer buffer;
    result = vkCreateBuffer(device, &buffer_info, NULL, &buffer);
    if (result != VK_SUCCESS) {
        vkDestroyDevice(device, NULL);
        vkDestroyInstance(instance, NULL);
        return fail_vk("vkCreateBuffer", result);
    }
    VkMemoryRequirements memory_requirements;
    vkGetBufferMemoryRequirements(device, buffer, &memory_requirements);
    uint32_t memory_type = find_memory_type(
        physical_device,
        memory_requirements.memoryTypeBits,
        VK_MEMORY_PROPERTY_HOST_VISIBLE_BIT | VK_MEMORY_PROPERTY_HOST_COHERENT_BIT);
    if (memory_type == UINT32_MAX) {
        fprintf(stderr, "no host-visible coherent Vulkan memory type\n");
        vkDestroyBuffer(device, buffer, NULL);
        vkDestroyDevice(device, NULL);
        vkDestroyInstance(instance, NULL);
        return 1;
    }
    VkMemoryAllocateInfo allocation_info = {
        .sType = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO,
        .allocationSize = memory_requirements.size,
        .memoryTypeIndex = memory_type,
    };
    VkDeviceMemory memory;
    result = vkAllocateMemory(device, &allocation_info, NULL, &memory);
    if (result != VK_SUCCESS) {
        vkDestroyBuffer(device, buffer, NULL);
        vkDestroyDevice(device, NULL);
        vkDestroyInstance(instance, NULL);
        return fail_vk("vkAllocateMemory", result);
    }
    result = vkBindBufferMemory(device, buffer, memory, 0);
    if (result != VK_SUCCESS) {
        vkFreeMemory(device, memory, NULL);
        vkDestroyBuffer(device, buffer, NULL);
        vkDestroyDevice(device, NULL);
        vkDestroyInstance(instance, NULL);
        return fail_vk("vkBindBufferMemory", result);
    }

    VkDescriptorSetLayoutBinding layout_binding = {
        .binding = 0,
        .descriptorType = VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,
        .descriptorCount = 1,
        .stageFlags = VK_SHADER_STAGE_COMPUTE_BIT,
    };
    VkDescriptorSetLayoutCreateInfo layout_info = {
        .sType = VK_STRUCTURE_TYPE_DESCRIPTOR_SET_LAYOUT_CREATE_INFO,
        .bindingCount = 1,
        .pBindings = &layout_binding,
    };
    VkDescriptorSetLayout descriptor_layout;
    result = vkCreateDescriptorSetLayout(device, &layout_info, NULL, &descriptor_layout);
    if (result != VK_SUCCESS) {
        return fail_vk("vkCreateDescriptorSetLayout", result);
    }
    VkPipelineLayoutCreateInfo pipeline_layout_info = {
        .sType = VK_STRUCTURE_TYPE_PIPELINE_LAYOUT_CREATE_INFO,
        .setLayoutCount = 1,
        .pSetLayouts = &descriptor_layout,
    };
    VkPipelineLayout pipeline_layout;
    result = vkCreatePipelineLayout(device, &pipeline_layout_info, NULL, &pipeline_layout);
    if (result != VK_SUCCESS) {
        return fail_vk("vkCreatePipelineLayout", result);
    }

    size_t shader_size = 0;
    uint32_t *shader_code = read_shader(&shader_size);
    if (shader_code == NULL) {
        fprintf(stderr, "read compute shader failed\n");
        return 1;
    }
    VkShaderModuleCreateInfo shader_info = {
        .sType = VK_STRUCTURE_TYPE_SHADER_MODULE_CREATE_INFO,
        .codeSize = shader_size,
        .pCode = shader_code,
    };
    VkShaderModule shader_module;
    result = vkCreateShaderModule(device, &shader_info, NULL, &shader_module);
    free(shader_code);
    if (result != VK_SUCCESS) {
        return fail_vk("vkCreateShaderModule", result);
    }
    VkPipelineShaderStageCreateInfo shader_stage = {
        .sType = VK_STRUCTURE_TYPE_PIPELINE_SHADER_STAGE_CREATE_INFO,
        .stage = VK_SHADER_STAGE_COMPUTE_BIT,
        .module = shader_module,
        .pName = "main",
    };
    VkComputePipelineCreateInfo pipeline_info = {
        .sType = VK_STRUCTURE_TYPE_COMPUTE_PIPELINE_CREATE_INFO,
        .stage = shader_stage,
        .layout = pipeline_layout,
    };
    VkPipeline pipeline;
    result = vkCreateComputePipelines(device, VK_NULL_HANDLE, 1, &pipeline_info, NULL, &pipeline);
    if (result != VK_SUCCESS) {
        return fail_vk("vkCreateComputePipelines", result);
    }

    VkDescriptorPoolSize pool_size = {
        .type = VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,
        .descriptorCount = 1,
    };
    VkDescriptorPoolCreateInfo pool_info = {
        .sType = VK_STRUCTURE_TYPE_DESCRIPTOR_POOL_CREATE_INFO,
        .maxSets = 1,
        .poolSizeCount = 1,
        .pPoolSizes = &pool_size,
    };
    VkDescriptorPool descriptor_pool;
    result = vkCreateDescriptorPool(device, &pool_info, NULL, &descriptor_pool);
    if (result != VK_SUCCESS) {
        return fail_vk("vkCreateDescriptorPool", result);
    }
    VkDescriptorSetAllocateInfo set_info = {
        .sType = VK_STRUCTURE_TYPE_DESCRIPTOR_SET_ALLOCATE_INFO,
        .descriptorPool = descriptor_pool,
        .descriptorSetCount = 1,
        .pSetLayouts = &descriptor_layout,
    };
    VkDescriptorSet descriptor_set;
    result = vkAllocateDescriptorSets(device, &set_info, &descriptor_set);
    if (result != VK_SUCCESS) {
        return fail_vk("vkAllocateDescriptorSets", result);
    }
    VkDescriptorBufferInfo descriptor_buffer = {
        .buffer = buffer,
        .offset = 0,
        .range = buffer_size,
    };
    VkWriteDescriptorSet descriptor_write = {
        .sType = VK_STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET,
        .dstSet = descriptor_set,
        .dstBinding = 0,
        .descriptorCount = 1,
        .descriptorType = VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,
        .pBufferInfo = &descriptor_buffer,
    };
    vkUpdateDescriptorSets(device, 1, &descriptor_write, 0, NULL);

    VkCommandPoolCreateInfo command_pool_info = {
        .sType = VK_STRUCTURE_TYPE_COMMAND_POOL_CREATE_INFO,
        .queueFamilyIndex = queue_family,
    };
    VkCommandPool command_pool;
    result = vkCreateCommandPool(device, &command_pool_info, NULL, &command_pool);
    if (result != VK_SUCCESS) {
        return fail_vk("vkCreateCommandPool", result);
    }
    VkCommandBufferAllocateInfo command_buffer_info = {
        .sType = VK_STRUCTURE_TYPE_COMMAND_BUFFER_ALLOCATE_INFO,
        .commandPool = command_pool,
        .level = VK_COMMAND_BUFFER_LEVEL_PRIMARY,
        .commandBufferCount = 1,
    };
    VkCommandBuffer command_buffer;
    result = vkAllocateCommandBuffers(device, &command_buffer_info, &command_buffer);
    if (result != VK_SUCCESS) {
        return fail_vk("vkAllocateCommandBuffers", result);
    }
    VkCommandBufferBeginInfo begin_info = {
        .sType = VK_STRUCTURE_TYPE_COMMAND_BUFFER_BEGIN_INFO,
        .flags = VK_COMMAND_BUFFER_USAGE_ONE_TIME_SUBMIT_BIT,
    };
    result = vkBeginCommandBuffer(command_buffer, &begin_info);
    if (result != VK_SUCCESS) {
        return fail_vk("vkBeginCommandBuffer", result);
    }
    vkCmdBindPipeline(command_buffer, VK_PIPELINE_BIND_POINT_COMPUTE, pipeline);
    vkCmdBindDescriptorSets(command_buffer, VK_PIPELINE_BIND_POINT_COMPUTE, pipeline_layout, 0, 1, &descriptor_set, 0, NULL);
    vkCmdDispatch(command_buffer, 1, 1, 1);
    result = vkEndCommandBuffer(command_buffer);
    if (result != VK_SUCCESS) {
        return fail_vk("vkEndCommandBuffer", result);
    }

    VkFenceCreateInfo fence_info = {.sType = VK_STRUCTURE_TYPE_FENCE_CREATE_INFO};
    VkFence fence;
    result = vkCreateFence(device, &fence_info, NULL, &fence);
    if (result != VK_SUCCESS) {
        return fail_vk("vkCreateFence", result);
    }
    VkSubmitInfo submit_info = {
        .sType = VK_STRUCTURE_TYPE_SUBMIT_INFO,
        .commandBufferCount = 1,
        .pCommandBuffers = &command_buffer,
    };
    result = vkQueueSubmit(queue, 1, &submit_info, fence);
    if (result != VK_SUCCESS) {
        return fail_vk("vkQueueSubmit", result);
    }
    result = vkWaitForFences(device, 1, &fence, VK_TRUE, 5000000000ULL);
    if (result != VK_SUCCESS) {
        return fail_vk("vkWaitForFences", result);
    }

    uint32_t *values = NULL;
    result = vkMapMemory(device, memory, 0, buffer_size, 0, (void **)&values);
    if (result != VK_SUCCESS) {
        return fail_vk("vkMapMemory", result);
    }
    for (uint32_t i = 0; i < VALUE_COUNT; i++) {
        uint32_t expected = i * 3u + 7u;
        if (values[i] != expected) {
            fprintf(stderr, "compute mismatch at %u: got %u, want %u\n", i, values[i], expected);
            vkUnmapMemory(device, memory);
            return 1;
        }
    }
    vkUnmapMemory(device, memory);

    printf("venus compute probe ok: device=%s values=%d\n", physical_properties.deviceName, VALUE_COUNT);

    vkDestroyFence(device, fence, NULL);
    vkDestroyCommandPool(device, command_pool, NULL);
    vkDestroyDescriptorPool(device, descriptor_pool, NULL);
    vkDestroyPipeline(device, pipeline, NULL);
    vkDestroyShaderModule(device, shader_module, NULL);
    vkDestroyPipelineLayout(device, pipeline_layout, NULL);
    vkDestroyDescriptorSetLayout(device, descriptor_layout, NULL);
    vkFreeMemory(device, memory, NULL);
    vkDestroyBuffer(device, buffer, NULL);
    vkDestroyDevice(device, NULL);
    vkDestroyInstance(instance, NULL);
    return 0;
}
